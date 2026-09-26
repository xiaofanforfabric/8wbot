import axios from 'axios'

// 创建 axios 实例
const request = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL || 'http://localhost:8888',
  timeout: 30000,
  headers: {
    'Content-Type': 'application/json'
  }
})

// 请求拦截器 - 自动添加 token
request.interceptors.request.use(
  config => {
    const token = localStorage.getItem('8wbot_token') || getCookie('8wbot_token')
    if (token) {
      config.headers['Authorization'] = `Bearer ${token}`
    }
    return config
  },
  error => {
    return Promise.reject(error)
  }
)

// 响应拦截器 - 统一处理错误
request.interceptors.response.use(
  response => {
    const res = response.data
    
    // 兼容旧 API 的 code 字段
    if (res.code && String(res.code) !== '200' && Number(res.code) !== 200) {
      return Promise.reject(new Error(res.msg || res.message || 'Error'))
    }
    
    return res
  },
  error => {
    console.error('API Error:', error)
    
    if (error.response?.status === 401) {
      // Token 过期，清除并跳转登录
      localStorage.removeItem('8wbot_token')
      document.cookie = '8wbot_token=; expires=Thu, 01 Jan 1970 00:00:00 UTC; path=/;'
      if (window.location.pathname !== '/login') {
        window.location.href = '/login'
      }
    }
    
    return Promise.reject(error)
  }
)

// Cookie 工具函数
function getCookie(name) {
  const match = document.cookie.match(new RegExp('(^| )' + name + '=([^;]+)'))
  return match ? decodeURIComponent(match[2]) : ''
}

export default request
