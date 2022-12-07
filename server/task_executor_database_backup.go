package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/bytebase/bytebase/api"
	"github.com/bytebase/bytebase/common/log"
	"github.com/bytebase/bytebase/plugin/db"
	bbs3 "github.com/bytebase/bytebase/plugin/storage/s3"
	"github.com/bytebase/bytebase/server/component/config"
	"github.com/bytebase/bytebase/server/component/dbfactory"
	"github.com/bytebase/bytebase/store"
)

const (
	// Do not dump backup file when the available file system space is less than 500MB.
	minAvailableFSBytes = 500 * 1024 * 1024
)

// NewDatabaseBackupTaskExecutor creates a new database backup task executor.
func NewDatabaseBackupTaskExecutor(store *store.Store, dbFactory *dbfactory.DBFactory, s3Client *bbs3.Client, profile config.Profile) TaskExecutor {
	return &DatabaseBackupTaskExecutor{
		store:     store,
		dbFactory: dbFactory,
		s3Client:  s3Client,
		profile:   profile,
	}
}

// DatabaseBackupTaskExecutor is the task executor for database backup.
type DatabaseBackupTaskExecutor struct {
	store     *store.Store
	dbFactory *dbfactory.DBFactory
	s3Client  *bbs3.Client
	profile   config.Profile
}

// RunOnce will run database backup once.
func (exec *DatabaseBackupTaskExecutor) RunOnce(ctx context.Context, task *api.Task) (terminated bool, result *api.TaskRunResultPayload, err error) {
	payload := &api.TaskDatabaseBackupPayload{}
	if err := json.Unmarshal([]byte(task.Payload), payload); err != nil {
		return true, nil, errors.Wrap(err, "invalid database backup payload")
	}

	backup, err := exec.store.GetBackupByID(ctx, payload.BackupID)
	if err != nil {
		return true, nil, errors.Wrapf(err, "failed to find backup with ID %d", payload.BackupID)
	}
	if backup == nil {
		return true, nil, errors.Errorf("backup %v not found", payload.BackupID)
	}

	if backup.StorageBackend == api.BackupStorageBackendLocal {
		backupFileDir := filepath.Dir(filepath.Join(exec.profile.DataDir, backup.Path))
		availableBytes, err := getAvailableFSSpace(backupFileDir)
		if err != nil {
			return true, nil, errors.Wrapf(err, "failed to get available file system space, backup file dir is %s", backupFileDir)
		}
		if availableBytes < minAvailableFSBytes {
			return true, nil, errors.Errorf("the available file system space %dMB is less than the minimal threshold %dMB", availableBytes/1024/1024, minAvailableFSBytes/1024/1024)
		}
	}

	log.Debug("Start database backup.", zap.String("instance", task.Instance.Name), zap.String("database", task.Database.Name), zap.String("backup", backup.Name))
	backupPayload, backupErr := exec.backupDatabase(ctx, exec.dbFactory, exec.s3Client, exec.profile, task.Instance, task.Database.Name, backup)
	backupStatus := string(api.BackupStatusDone)
	comment := ""
	if backupErr != nil {
		backupStatus = string(api.BackupStatusFailed)
		comment = backupErr.Error()
		if err := removeLocalBackupFile(exec.profile.DataDir, backup); err != nil {
			log.Warn(err.Error())
		}
	}
	backupPatch := api.BackupPatch{
		ID:        backup.ID,
		Status:    &backupStatus,
		UpdaterID: api.SystemBotID,
		Comment:   &comment,
		Payload:   &backupPayload,
	}

	if _, err := exec.store.PatchBackup(ctx, &backupPatch); err != nil {
		return true, nil, errors.Wrap(err, "failed to patch backup")
	}

	if backupErr != nil {
		return true, nil, backupErr
	}

	return true, &api.TaskRunResultPayload{
		Detail: fmt.Sprintf("Backup database %q", task.Database.Name),
	}, nil
}

func removeLocalBackupFile(dataDir string, backup *api.Backup) error {
	if backup.StorageBackend != api.BackupStorageBackendLocal {
		return nil
	}
	backupFilePath := getBackupAbsFilePath(dataDir, backup.DatabaseID, backup.Name)
	if err := os.Remove(backupFilePath); err != nil {
		return errors.Wrapf(err, "failed to delete the local backup file %s", backupFilePath)
	}
	return nil
}

// getAvailableFSSpace gets the free space of the mounted filesystem.
// path is the pathname of any file within the mounted filesystem.
// It calls syscall statfs under the hood.
// Returns available space in bytes.
func getAvailableFSSpace(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, errors.Wrap(err, "failed to call syscall statfs")
	}
	// Ref: https://man7.org/linux/man-pages/man2/statfs.2.html
	// Bavail: Free blocks available to unprivileged user.
	// Bsize: Optimal transfer block size.
	return stat.Bavail * uint64(stat.Bsize), nil
}

func dumpBackupFile(ctx context.Context, driver db.Driver, databaseName, backupFilePath string) (string, error) {
	backupFile, err := os.Create(backupFilePath)
	if err != nil {
		return "", errors.Errorf("failed to open backup path %q", backupFilePath)
	}
	defer backupFile.Close()
	payload, err := driver.Dump(ctx, databaseName, backupFile, false /* schemaOnly */)
	if err != nil {
		return "", errors.Wrapf(err, "failed to dump database %q to local backup file %q", databaseName, backupFilePath)
	}
	return payload, nil
}

// backupDatabase will take a backup of a database.
func (*DatabaseBackupTaskExecutor) backupDatabase(ctx context.Context, dbFactory *dbfactory.DBFactory, s3Client *bbs3.Client, profile config.Profile, instance *api.Instance, databaseName string, backup *api.Backup) (string, error) {
	driver, err := dbFactory.GetAdminDatabaseDriver(ctx, instance, databaseName)
	if err != nil {
		return "", err
	}
	defer driver.Close(ctx)

	backupFilePathLocal := filepath.Join(profile.DataDir, backup.Path)
	payload, err := dumpBackupFile(ctx, driver, databaseName, backupFilePathLocal)
	if err != nil {
		return "", errors.Wrapf(err, "failed to dump backup file %q", backupFilePathLocal)
	}

	switch backup.StorageBackend {
	case api.BackupStorageBackendLocal:
		return payload, nil
	case api.BackupStorageBackendS3:
		log.Debug("Uploading backup to s3 bucket.", zap.String("bucket", s3Client.GetBucket()), zap.String("path", backupFilePathLocal))
		bucketFileToUpload, err := os.Open(backupFilePathLocal)
		if err != nil {
			return "", errors.Wrapf(err, "failed to open backup file %q for uploading to s3 bucket", backupFilePathLocal)
		}
		defer bucketFileToUpload.Close()

		if _, err := s3Client.UploadObject(ctx, backup.Path, bucketFileToUpload); err != nil {
			return "", errors.Wrapf(err, "failed to upload backup to AWS S3")
		}
		log.Debug("Successfully uploaded backup to s3 bucket.")

		if err := os.Remove(backupFilePathLocal); err != nil {
			log.Warn("Failed to remove the local backup file after uploading to s3 bucket.", zap.String("path", backupFilePathLocal), zap.Error(err))
		} else {
			log.Debug("Successfully removed the local backup file after uploading to s3 bucket.", zap.String("path", backupFilePathLocal))
		}
		return payload, nil
	default:
		return "", errors.Errorf("backup to %s not implemented yet", backup.StorageBackend)
	}
}

// Get backup dir relative to the data dir.
func getBackupRelativeDir(databaseID int) string {
	return filepath.Join("backup", "db", fmt.Sprintf("%d", databaseID))
}

func getBackupRelativeFilePath(databaseID int, name string) string {
	dir := getBackupRelativeDir(databaseID)
	return filepath.Join(dir, fmt.Sprintf("%s.sql", name))
}

func getBackupAbsFilePath(dataDir string, databaseID int, name string) string {
	path := getBackupRelativeFilePath(databaseID, name)
	return filepath.Join(dataDir, path)
}

// Create backup directory for database.
func createBackupDirectory(dataDir string, databaseID int) error {
	dir := getBackupRelativeDir(databaseID)
	absDir := filepath.Join(dataDir, dir)
	return os.MkdirAll(absDir, os.ModePerm)
}
