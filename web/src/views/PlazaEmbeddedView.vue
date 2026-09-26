<script setup lang="ts">
/**
 * 模型广场（控制台内嵌版）：用户门户与管理后台共用同一个页面组件。
 *
 * 意图（Why）：
 *   在控制台里点"模型广场"却跳到公开页，会把人从当前外壳（侧边栏、身份、上下文）
 *   里甩出去，回来还得重新找位置 —— 这是最影响操作连贯性的体验问题。
 *   因此广场在登录后以"普通控制台页面"的形态就地渲染：
 *   外壳不变、侧边栏不变、面包屑不变，只换内容区。
 *
 *   为什么门户和后台复用同一个文件：
 *   两侧看到的是同一份数据、同一套筛选，差异只在"外壳"，
 *   而外壳由父路由的 layout 决定（/console/models 与 /admin/models），
 *   所以页面本体只写一次就够，不会出现两份逐渐分叉的实现。
 *
 * 流转（Flow）：
 *   router → /console/models 或 /admin/models → 父布局（AppShell）→ 本页 → ModelPlazaBoard
 *
 * 扩展（Extend）：
 *   需要给管理员展示"额外视角"（如各渠道的模型覆盖情况）时，
 *   在此按 auth.isAdmin 追加一段，不要把差异塞进 ModelPlazaBoard。
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import AppIcon from '@/components/AppIcon.vue'
import ModelPlazaBoard from '@/components/ModelPlazaBoard.vue'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()

/**
 * 「访问令牌」的落点随外壳而变：后台管理员应留在 /admin，
 * 否则点一下就从管理后台掉到用户门户，导航高亮与上下文都会断。
 */
const tokenTarget = computed(() => (auth.isAdmin ? '/admin/tokens' : '/console/tokens'))
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div class="max-w-2xl">
        <h2 class="page-title">模型广场</h2>
        <p class="page-desc">
          本站对外提供的全部模型、分组与价格。左侧按分组 / 厂商 / 状态筛选，
          点击任意卡片可就地查看价格与调用命令，无需离开本页。
        </p>
      </div>
      <div class="toolbar">
        <RouterLink :to="tokenTarget" class="btn btn-secondary btn-sm">
          <AppIcon name="key" :size="15" />
          访问令牌
        </RouterLink>
        <RouterLink v-if="auth.isAdmin" to="/admin/channels" class="btn btn-secondary btn-sm">
          <AppIcon name="server" :size="15" />
          渠道管理
        </RouterLink>
      </div>
    </div>

    <ModelPlazaBoard />
  </div>
</template>
