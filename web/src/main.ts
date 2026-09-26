/**
 * 应用入口：装配 Pinia、i18n、路由、全局样式与全局宿主组件。
 *
 * 意图（Why）：
 *   main.ts 只做「装配」，不含业务逻辑（与后端 cmd/aqua/main.go 的思路一致）；
 *   另外在此处注入 401 失效回调，把「网络层」与「路由/状态层」连起来，
 *   避免 api/client.ts 直接依赖 router 造成循环引用。
 *
 * 流转（Flow）：
 *   index.html → main.ts
 *     → 创建 Pinia / Router
 *     → 安装 i18n（语言检测与 <html lang/dir> 已在 i18n/index.ts 内完成）
 *     → 注入未授权回调（清登录态 + 跳登录页）
 *     → 预取站点信息（失败不阻断，页面自行提示）
 *     → mount(App.vue)
 *
 * 扩展（Extend）：
 *   新增全局插件在此 use()；新增全局浮层组件请挂到 App.vue。
 */
import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from './App.vue'
import { setUnauthorizedHandler } from './api/client'
import { i18n } from './i18n'
import { router } from './router'
import { useAuthStore } from './stores/auth'
import { useSiteStore } from './stores/site'
import './style.css'

const app = createApp(App)
const pinia = createPinia()

app.use(pinia)
app.use(i18n)
app.use(router)

// 会话失效的集中处理：清本地登录态 + 带 redirect 跳登录页
// （在路由守卫之外注册，保证任意页面的任意请求都能触发）
setUnauthorizedHandler(() => {
  const auth = useAuthStore(pinia)
  auth.clearLocal()
  const current = router.currentRoute.value
  if (current.name !== 'login') {
    void router.replace({ name: 'login', query: { redirect: current.fullPath } })
  }
})

// 预取站点信息：落地页首屏即需要，提前发起可减少一次串行等待
void useSiteStore(pinia).load()

app.mount('#app')
