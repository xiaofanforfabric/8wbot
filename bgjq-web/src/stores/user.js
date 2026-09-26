import { defineStore } from 'pinia'
import { ref } from 'vue'
import { getUserData } from '../api'

export const useUserStore = defineStore('user', () => {
  const user = ref(null)
  const token = ref('')
  const loading = ref(false)

  // 从 localStorage/cookie 恢复 token
  function restoreToken() {
    const savedToken = localStorage.getItem('8wbot_token') || getCookie('8wbot_token')
    if (savedToken) {
      token.value = savedToken
    }
  }

  // 设置 token
  function setToken(newToken) {
    token.value = newToken
    localStorage.setItem('8wbot_token', newToken)
    
    // 设置 cookie (7天过期)
    const d = new Date()
    d.setTime(d.getTime() + 7 * 24 * 60 * 60 * 1000)
    document.cookie = `8wbot_token=${encodeURIComponent(newToken)}; expires=${d.toUTCString()}; path=/; SameSite=Lax`
  }

  // 清除 token
  function clearToken() {
    token.value = ''
    user.value = null
    localStorage.removeItem('8wbot_token')
    document.cookie = '8wbot_token=; expires=Thu, 01 Jan 1970 00:00:00 UTC; path=/;'
  }

  // 获取用户信息
  async function fetchUserInfo() {
    if (!token.value) {
      restoreToken()
    }
    
    if (!token.value) {
      return null
    }

    loading.value = true
    try {
      const res = await getUserData(token.value)
      if (res.code === 200 || res.code === '200') {
        user.value = res.user_info || res.data
        return user.value
      } else {
        clearToken()
        return null
      }
    } catch (error) {
      console.error('获取用户信息失败:', error)
      clearToken()
      return null
    } finally {
      loading.value = false
    }
  }

  // 登出
  function logout() {
    clearToken()
    window.location.href = '/login'
  }

  // Cookie 工具函数
  function getCookie(name) {
    const match = document.cookie.match(new RegExp('(^| )' + name + '=([^;]+)'))
    return match ? decodeURIComponent(match[2]) : ''
  }

  return {
    user,
    token,
    loading,
    setToken,
    clearToken,
    fetchUserInfo,
    logout,
    restoreToken
  }
})
