import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: '/',
      name: 'home',
      component: () => import('../views/HomePage.vue')
    },
    {
      path: '/login',
      name: 'login',
      component: () => import('../views/LoginPage.vue')
    },
    {
      path: '/user',
      name: 'user',
      component: () => import('../views/UserPage.vue'),
      meta: { requiresAuth: true }
    },
    {
      path: '/console/:botname?',
      name: 'console',
      component: () => import('../views/ConsolePage.vue'),
      meta: { requiresAuth: true }
    },
    {
      path: '/create-bot',
      name: 'create-bot',
      component: () => import('../views/CreateBotPage.vue'),
      meta: { requiresAuth: true }
    }
  ]
})

// 路由守卫 - 仅在访问受保护页面时检查，允许用户随时自由进入 /login
router.beforeEach((to, from, next) => {
  const token = localStorage.getItem('8wbot_token') || getCookie('8wbot_token')
  
  if (to.meta.requiresAuth && !token) {
    // 需要登录但没有有效 token，去登录页
    next({ name: 'login', query: { redirect: to.fullPath } })
  } else {
    // 放行所有页面访问，包括 /login
    next()
  }
})

function getCookie(name) {
  const match = document.cookie.match(new RegExp('(^| )' + name + '=([^;]+)'))
  return match ? decodeURIComponent(match[2]) : ''
}

export default router
