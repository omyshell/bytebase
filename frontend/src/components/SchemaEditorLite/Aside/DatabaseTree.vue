<template>
  <div
    class="w-full h-full pl-1 pr-2 flex flex-col gap-y-2 relative overflow-y-hidden"
  >
    <div class="w-full sticky top-0 pt-2 h-12 bg-white z-10">
      <NInput
        v-model:value="searchPattern"
        :placeholder="$t('schema-editor.search-database-and-table')"
      >
        <template #prefix>
          <heroicons-outline:search class="w-4 h-auto text-gray-300" />
        </template>
      </NInput>
    </div>
    <div
      ref="treeContainerElRef"
      class="schema-editor-database-tree flex-1 pb-1 text-sm overflow-hidden select-none"
      :data-height="treeContainerHeight"
    >
      <NTree
        v-if="treeContainerHeight > 0"
        ref="treeRef"
        block-line
        virtual-scroll
        :style="{
          height: `${treeContainerHeight}px`,
        }"
        :data="treeDataRef"
        :pattern="searchPattern"
        :render-prefix="renderPrefix"
        :render-label="renderLabel"
        :render-suffix="renderSuffix"
        :node-props="nodeProps"
        :expanded-keys="expandedKeysRef"
        :selected-keys="selectedKeysRef"
        :show-irrelevant-nodes="false"
        :theme-overrides="{ nodeHeight: '28px' }"
        @update:expanded-keys="handleExpandedKeysChange"
      />
      <NDropdown
        trigger="manual"
        placement="bottom-start"
        :show="contextMenu.showDropdown"
        :options="contextMenuOptions"
        :x="contextMenu.clientX"
        :y="contextMenu.clientY"
        to="body"
        @select="handleContextMenuDropdownSelect"
        @clickoutside="handleDropdownClickoutside"
      />
    </div>
  </div>

  <SchemaNameModal
    v-if="state.schemaNameModalContext !== undefined"
    :database="state.schemaNameModalContext.db"
    :metadata="state.schemaNameModalContext.database"
    @close="state.schemaNameModalContext = undefined"
  />

  <TableNameModal
    v-if="state.tableNameModalContext !== undefined"
    :database="state.tableNameModalContext.db"
    :metadata="state.tableNameModalContext.database"
    :schema="state.tableNameModalContext.schema"
    :table="state.tableNameModalContext.table"
    @close="state.tableNameModalContext = undefined"
  />
</template>

<script lang="ts" setup>
import { useElementSize } from "@vueuse/core";
import { debounce, escape, head } from "lodash-es";
import {
  TreeOption,
  NEllipsis,
  NInput,
  NDropdown,
  NTree,
  DropdownOption,
} from "naive-ui";
import { computed, onMounted, watch, ref, h, reactive, nextTick } from "vue";
import { useI18n } from "vue-i18n";
import EllipsisIcon from "~icons/heroicons-solid/ellipsis-horizontal";
import { DatabaseIcon, SchemaIcon, TableIcon } from "@/components/Icon";
import { InstanceV1EngineIcon } from "@/components/v2";
import { useEmitteryEventListener } from "@/composables/useEmitteryEventListener";
import { ComposedDatabase, ComposedInstance } from "@/types";
import { Engine } from "@/types/proto/v1/common";
import {
  DatabaseMetadata,
  SchemaMetadata,
  TableMetadata,
} from "@/types/proto/v1/database_service";
import { getHighlightHTMLByKeyWords, groupBy, isDescendantOf } from "@/utils";
import SchemaNameModal from "../Modals/SchemaNameModal.vue";
import TableNameModal from "../Modals/TableNameModal.vue";
import { useSchemaEditorContext } from "../context";
import { keyForResource, keyForResourceName } from "../context/common";
import { engineHasSchema } from "../engine-specs";
import {
  BaseTreeNode,
  TreeNodeForDatabase as CommonDatabaseNode,
  TreeNodeForSchema as CommonSchemaNode,
  TreeNodeForTable as CommonTableNode,
  TreeNodeForColumn as CommonColumnNode,
} from "./types";

interface TreeNodeForInstance extends BaseTreeNode {
  type: "instance";
  instance: ComposedInstance;
  children: TreeNodeForDatabase[];
}

interface TreeNodeForDatabase extends CommonDatabaseNode {
  parent: TreeNodeForInstance;
  instance: ComposedInstance;
  children: TreeNodeForSchema[];
}
interface TreeNodeForSchema extends CommonSchemaNode {
  parent: TreeNodeForDatabase;
  instance: ComposedInstance;
  children: TreeNodeForTable[];
}
interface TreeNodeForTable extends CommonTableNode {
  parent: TreeNodeForSchema;
  instance: ComposedInstance;
  children: TreeNodeForColumn[];
}
interface TreeNodeForColumn extends CommonColumnNode {
  parent: TreeNodeForTable;
  instance: ComposedInstance;
  isLeaf: true;
}

type TreeNode =
  | TreeNodeForInstance
  | TreeNodeForDatabase
  | TreeNodeForSchema
  | TreeNodeForTable
  | TreeNodeForColumn;

interface TreeContextMenu {
  showDropdown: boolean;
  clientX: number;
  clientY: number;
  treeNode?: TreeNode;
}

interface LocalState {
  shouldRelocateTreeNode: boolean;
  schemaNameModalContext?: {
    db: ComposedDatabase;
    database: DatabaseMetadata;
    schema?: SchemaMetadata;
  };
  tableNameModalContext?: {
    db: ComposedDatabase;
    database: DatabaseMetadata;
    schema: SchemaMetadata;
    table?: TableMetadata;
  };
}

const { t } = useI18n();
const {
  events,
  targets,
  currentTab,
  addTab,
  markEditStatus,
  removeEditStatus,
  getSchemaStatus,
  getTableStatus,
  getColumnStatus,
  queuePendingScrollToColumn,
} = useSchemaEditorContext();
const state = reactive<LocalState>({
  shouldRelocateTreeNode: false,
});

const contextMenu = reactive<TreeContextMenu>({
  showDropdown: false,
  clientX: 0,
  clientY: 0,
  treeNode: undefined,
});
const treeContainerElRef = ref<HTMLElement>();
const { height: treeContainerHeight } = useElementSize(
  treeContainerElRef,
  undefined,
  {
    box: "content-box",
  }
);
const treeRef = ref<InstanceType<typeof NTree>>();
const searchPattern = ref("");
const expandedKeysRef = ref<string[]>([]);
const selectedKeysRef = ref<string[]>([]);
const treeDataRef = ref<TreeNode[]>([]);
const treeNodeMap = new Map<string, TreeNode>();

const contextMenuOptions = computed(() => {
  const treeNode = contextMenu.treeNode;
  if (!treeNode) return [];
  const { engine } = treeNode.instance;
  if (treeNode.type === "database") {
    const options: DropdownOption[] = [];
    if (engineHasSchema(engine)) {
      options.push({
        key: "create-schema",
        label: t("schema-editor.actions.create-schema"),
      });
    } else {
      options.push({
        key: "create-table",
        label: t("schema-editor.actions.create-table"),
      });
    }
    return options;
  } else if (treeNode.type === "schema") {
    const options: DropdownOption[] = [];
    if (engine === Engine.POSTGRES) {
      const status = getSchemaStatus(treeNode.db, treeNode.metadata);
      if (status === "dropped") {
        options.push({
          key: "restore",
          label: t("schema-editor.actions.restore"),
        });
      } else {
        options.push({
          key: "create-table",
          label: t("schema-editor.actions.create-table"),
        });
        options.push({
          key: "rename",
          label: t("schema-editor.actions.rename"),
        });
        options.push({
          key: "drop-schema",
          label: t("schema-editor.actions.drop-schema"),
        });
      }
    }
    return options;
  } else if (treeNode.type === "table") {
    const options: DropdownOption[] = [];
    const status = getTableStatus(treeNode.db, treeNode.metadata);
    if (status === "dropped") {
      options.push({
        key: "restore",
        label: t("schema-editor.actions.restore"),
      });
    } else {
      options.push({
        key: "rename",
        label: t("schema-editor.actions.rename"),
      });
      options.push({
        key: "drop",
        label: t("schema-editor.actions.drop-table"),
      });
    }
    return options;
  }

  return [];
});

const upsertExpandedKeys = (key: string) => {
  if (expandedKeysRef.value.includes(key)) return;
  expandedKeysRef.value.push(key);
};
const expandNodeRecursively = (node: TreeNode) => {
  if (node.type === "column") {
    // column nodes are not expandable
    expandNodeRecursively(node.parent);
  }
  if (node.type === "table") {
    const key = keyForResource(node.db, node.metadata);
    upsertExpandedKeys(key);
    expandNodeRecursively(node.parent);
  }
  if (node.type === "schema") {
    const key = keyForResource(node.db, node.metadata);
    upsertExpandedKeys(key);
    expandNodeRecursively(node.parent);
  }
  if (node.type === "database") {
    const key = node.db.name;
    upsertExpandedKeys(key);
    expandNodeRecursively(node.parent);
  }
  if (node.type === "instance") {
    const key = node.instance.name;
    upsertExpandedKeys(key);
  }
};
const buildDatabaseTreeData = (openFirstChild: boolean) => {
  const groupedByInstance = groupBy(
    targets.value,
    (target) => target.database.instance
  );
  treeNodeMap.clear();
  const treeNodeList = Array.from(groupedByInstance).map(([_, targets]) => {
    const instance = targets[0].database.instanceEntity;
    const instanceNode: TreeNodeForInstance = {
      type: "instance",
      key: instance.name,
      label: instance.title,
      isLeaf: false,
      instance,
      children: [],
    };
    instanceNode.children = targets.map((target) => {
      const db = target.database;
      const databaseNode: TreeNodeForDatabase = {
        type: "database",
        key: db.name,
        parent: instanceNode,
        label: db.databaseName,
        isLeaf: false,
        instance,
        db,
        metadata: {
          database: target.metadata,
        },
        children: [],
      };
      databaseNode.children = target.metadata.schemas.map((schema) => {
        const metadata = {
          database: target.metadata,
          schema,
        };
        const schemaNode: TreeNodeForSchema = {
          type: "schema",
          parent: databaseNode,
          key: keyForResource(db, metadata),
          label: schema.name,
          isLeaf: false,
          instance,
          db,
          metadata,
          children: [],
        };
        schemaNode.children = schema.tables.map((table) => {
          const metadata = {
            database: target.metadata,
            schema,
            table,
          };
          const tableNode: TreeNodeForTable = {
            type: "table",
            parent: schemaNode,
            key: keyForResource(db, metadata),
            label: table.name,
            isLeaf: false,
            instance,
            db,
            metadata,
            children: [],
          };
          tableNode.children = table.columns.map((column) => {
            const metadata = {
              database: target.metadata,
              schema,
              table,
              column,
            };
            const columnNode: TreeNodeForColumn = {
              type: "column",
              key: keyForResource(db, {
                schema,
                table,
                column,
              }),
              parent: tableNode,
              label: column.name,
              children: [],
              isLeaf: true,
              db,
              instance,
              metadata,
            };
            treeNodeMap.set(columnNode.key, columnNode);
            return columnNode;
          });
          treeNodeMap.set(tableNode.key, tableNode);
          return tableNode;
        });
        treeNodeMap.set(schemaNode.key, schemaNode);
        return schemaNode;
      });
      treeNodeMap.set(databaseNode.key, databaseNode);
      return databaseNode;
    });
    treeNodeMap.set(instanceNode.key, instanceNode);
    return instanceNode;
  });

  treeDataRef.value = treeNodeList;

  if (openFirstChild) {
    const firstInstanceNode = head(treeNodeList) as
      | TreeNodeForInstance
      | undefined;
    const firstDatabaseNode = head(firstInstanceNode?.children);
    const firstSchemaNode = head(firstDatabaseNode?.children);
    if (firstSchemaNode) {
      nextTick(() => {
        // Auto expand the first tree node.
        openTabForTreeNode(firstSchemaNode);
      });
    }
  }
};
const debouncedBuildDatabaseTreeData = debounce(buildDatabaseTreeData, 100);
useEmitteryEventListener(events, "rebuild-tree", (params) => {
  debouncedBuildDatabaseTreeData(params.openFirstChild);
});

const tabWatchKey = computed(() => {
  const tab = currentTab.value;
  if (!tab) return undefined;
  if (tab.type === "database") {
    return keyForResourceName(tab.database.name, tab.selectedSchema);
  }
  return keyForResource(tab.database, tab.metadata);
});
// Sync tree expandedKeys and selectedKeys with tabs
watch(tabWatchKey, () => {
  requestAnimationFrame(() => {
    const tab = currentTab.value;
    if (!tab) {
      selectedKeysRef.value = [];
      return;
    }

    if (tab.type === "database") {
      const { database, selectedSchema: schema } = tab;
      if (schema) {
        const key = keyForResourceName(database.name, schema);
        const node = treeNodeMap.get(key);
        if (node) {
          expandNodeRecursively(node);
        }
        selectedKeysRef.value = [key];
      }
    } else if (tab.type === "table") {
      const {
        database,
        metadata: { schema, table },
      } = tab;
      const schemaKey = keyForResource(database, { schema });
      const schemaNode = treeNodeMap.get(schemaKey);
      if (schemaNode) {
        expandNodeRecursively(schemaNode);
      }
      const tableKey = keyForResource(database, { schema, table });
      selectedKeysRef.value = [tableKey];
    }

    if (state.shouldRelocateTreeNode) {
      nextTick(() => {
        treeRef.value?.scrollTo({
          key: selectedKeysRef.value[0],
        });
      });
    }
  });
});

const openTabForTreeNode = (node: TreeNode) => {
  state.shouldRelocateTreeNode = false;

  if (node.type === "column") {
    openTabForTreeNode(node.parent);
    queuePendingScrollToColumn({
      db: node.db,
      metadata: node.metadata,
    });
    return;
  } else if (node.type === "table") {
    expandNodeRecursively(node);
    addTab({
      type: "table",
      database: node.db,
      metadata: node.metadata,
    });
  } else if (node.type === "schema") {
    expandNodeRecursively(node);
    addTab({
      type: "database",
      database: node.db,
      metadata: node.metadata,
      selectedSchema: node.metadata.schema.name,
    });
  }

  state.shouldRelocateTreeNode = true;
};

onMounted(() => {
  buildDatabaseTreeData(/* openFirstChild */ true);
});

// Render prefix icons before label text.
const renderPrefix = ({ option }: { option: TreeOption }) => {
  const treeNode = option as TreeNode;

  if (treeNode.type === "instance") {
    const { instance } = treeNode;
    const children = [
      h(InstanceV1EngineIcon, {
        instance,
      }),
      h(
        "span",
        {
          class: "text-gray-500 text-sm",
        },
        `(${instance.environmentEntity.title})`
      ),
    ];

    return h("span", { class: "flex items-center gap-x-1" }, children);
  } else if (treeNode.type === "database") {
    return h("span", { class: "flex items-center gap-x-1" }, [
      h(DatabaseIcon, {
        class: "w-4 h-auto text-gray-400",
      }),
      h(
        "span",
        {
          class: "text-gray-500 text-sm",
        },
        `(${treeNode.db.effectiveEnvironmentEntity.title})`
      ),
    ]);
  } else if (treeNode.type === "schema") {
    return h(SchemaIcon, {
      class: "w-4 h-auto text-gray-400",
    });
  } else if (treeNode.type === "table") {
    return h(TableIcon, {
      class: "w-4 h-auto text-gray-400",
    });
  }
  return null;
};

// Render label text.
const renderLabel = ({ option }: { option: TreeOption }) => {
  const treeNode = option as TreeNode;
  const additionalClassList: string[] = ["select-none"];
  let label = treeNode.label;

  if (treeNode.type === "schema") {
    const { db, metadata } = treeNode;
    additionalClassList.push(getSchemaStatus(db, metadata));

    if (db.instanceEntity.engine !== Engine.POSTGRES) {
      label = t("db.tables");
    } else {
      label = metadata.schema.name;
    }
  } else if (treeNode.type === "table") {
    const { db, metadata } = treeNode;
    additionalClassList.push(getTableStatus(db, metadata));
    label = metadata.table.name;
  } else if (treeNode.type === "column") {
    const { db, metadata } = treeNode;
    additionalClassList.push(getColumnStatus(db, metadata));
    const { name } = metadata.column;
    if (name) {
      label = name;
    } else {
      label = `<${t("common.untitled")}>`;
      additionalClassList.push("text-control-placeholder italic");
    }
  }

  return h(
    NEllipsis,
    {
      class: additionalClassList.join(" "),
    },
    () => [
      h("span", {
        innerHTML: getHighlightHTMLByKeyWords(
          escape(label),
          escape(searchPattern.value)
        ),
      }),
    ]
  );
};

// Render a 'menu' icon in the right of the node
const renderSuffix = ({ option }: { option: TreeOption }) => {
  const treeNode = option as TreeNode;
  const icon = h(EllipsisIcon, {
    class: "w-4 h-auto text-gray-600",
    onClick: (e) => {
      handleShowDropdown(e, treeNode);
    },
  });
  const { engine } = treeNode.instance;
  if (treeNode.type === "database") {
    return icon;
  } else if (treeNode.type === "schema") {
    if (engine === Engine.POSTGRES) {
      return icon;
    }
  } else if (treeNode.type === "table") {
    return icon;
  }
  return null;
};

const handleShowDropdown = (e: MouseEvent, treeNode: TreeNode) => {
  e.preventDefault();
  e.stopPropagation();
  contextMenu.treeNode = treeNode;
  contextMenu.showDropdown = true;
  contextMenu.clientX = e.clientX;
  contextMenu.clientY = e.clientY;
  selectedKeysRef.value = [treeNode.key];
};

// Set event handler to tree nodes.
const nodeProps = ({ option }: { option: TreeOption }) => {
  const treeNode = option as TreeNode;
  return {
    async onClick(e: MouseEvent) {
      // Check if clicked on the content part.
      // And ignore the fold/unfold arrow.
      if (isDescendantOf(e.target as Element, ".n-tree-node-content")) {
        state.shouldRelocateTreeNode = false;
        if (treeNode.type === "instance") {
          expandNodeRecursively(treeNode);
        } else if (treeNode.type === "database") {
          expandNodeRecursively(treeNode);
          addTab({
            type: "database",
            database: treeNode.db,
            metadata: treeNode.metadata,
            selectedSchema: head(treeNode.metadata.database.schemas)?.name,
          });
        } else if (treeNode.type === "schema") {
          openTabForTreeNode(treeNode);
        } else if (treeNode.type === "table") {
          openTabForTreeNode(treeNode);
        } else if (treeNode.type === "column") {
          openTabForTreeNode(treeNode);
        }
      } else {
        nextTick(() => {
          selectedKeysRef.value = [];
        });
      }
    },
    oncontextmenu(e: MouseEvent) {
      handleShowDropdown(e, treeNode);
    },
  };
};

const handleContextMenuDropdownSelect = async (key: string) => {
  const treeNode = contextMenu.treeNode;
  if (!treeNode) return;
  if (treeNode.type === "database") {
    const engine = treeNode.instance.engine;
    if (key === "create-table") {
      if (!engineHasSchema(engine)) {
        const schema = head(treeNode.metadata.database.schemas);
        if (!schema) {
          return;
        }
        state.tableNameModalContext = {
          db: treeNode.db,
          database: treeNode.metadata.database,
          schema: schema,
          table: undefined,
        };
      }
    } else if (key === "create-schema") {
      if (engineHasSchema(engine)) {
        state.schemaNameModalContext = {
          db: treeNode.db,
          database: treeNode.metadata.database,
          schema: undefined,
        };
      }
    }
  } else if (treeNode.type === "schema") {
    if (key === "create-table") {
      state.tableNameModalContext = {
        db: treeNode.db,
        database: treeNode.metadata.database,
        schema: treeNode.metadata.schema,
        table: undefined,
      };
    } else if (key === "drop-schema") {
      markEditStatus(treeNode.db, treeNode.metadata, "dropped");
    } else if (key === "restore") {
      removeEditStatus(treeNode.db, treeNode.metadata, /* recursive */ false);
    }
  } else if (treeNode.type === "table") {
    if (key === "rename") {
      state.tableNameModalContext = {
        db: treeNode.db,
        database: treeNode.metadata.database,
        schema: treeNode.metadata.schema,
        table: treeNode.metadata.table,
      };
    } else if (key === "drop") {
      markEditStatus(treeNode.db, treeNode.metadata, "dropped");
    } else if (key === "restore") {
      removeEditStatus(treeNode.db, treeNode.metadata, /* recursive */ false);
    }
  }
  contextMenu.showDropdown = false;
};

const handleDropdownClickoutside = (e: MouseEvent) => {
  if (
    !isDescendantOf(e.target as Element, ".n-tree-node-wrapper") ||
    e.button !== 2
  ) {
    selectedKeysRef.value = [];
    contextMenu.showDropdown = false;
  }
};

const handleExpandedKeysChange = (expandedKeys: string[]) => {
  expandedKeysRef.value = expandedKeys;
};
</script>

<style lang="postcss" scoped>
.schema-editor-database-tree :deep(.n-tree-node-wrapper) {
  @apply !p-0;
}
.schema-editor-database-tree :deep(.n-tree-node-content) {
  @apply !pl-2 text-sm;
}
.schema-editor-database-tree :deep(.n-tree-node-indent) {
  width: 0.25rem;
}
.schema-editor-database-tree :deep(.n-tree-node-content__prefix) {
  @apply shrink-0 !mr-1;
}
.schema-editor-database-tree
  :deep(.n-tree-node-wrapper:hover .n-tree-node-content__suffix) {
  @apply !flex;
}
.schema-editor-database-tree
  :deep(.n-tree-node-wrapper
    .n-tree-node--selected
    .n-tree-node-content__suffix) {
  @apply !flex;
}
.schema-editor-database-tree :deep(.n-tree-node-switcher) {
  @apply px-0 !w-4 !h-7;
}
.schema-editor-database-tree :deep(.n-tree-node-content .created) {
  @apply text-green-700;
}
.schema-editor-database-tree :deep(.n-tree-node-content .dropped) {
  @apply text-red-700 line-through;
}
.schema-editor-database-tree :deep(.n-tree-node-content .updated) {
  @apply text-yellow-700;
}
</style>
