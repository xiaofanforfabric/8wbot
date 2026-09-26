import request from './request'

// ==================== 认证相关 ====================

/**
 * 动态验证码登录
 * @param {string} user_id - FanVerify UID
 * @param {string} verify_code - 动态验证码
 * @param {string} cap_token - Cap 验证 token
 */
export function login(data) {
  return request({
    url: '/api/login',
    method: 'POST',
    data
  })
}

/**
 * FanVerify 认证 (新版)
 * @param {string} uid - FanVerify UID
 * @param {string} pass_code - 动态验证码
 */
export function authWithFanVerify(data) {
  return request({
    url: '/api/dev/auth',
    method: 'POST',
    data
  })
}

/**
 * 生成 OTP 二维码
 * @param {string} cap_token - Cap 验证 token
 */
export function generateOTP(cap_token) {
  return request({
    url: '/api/login',
    method: 'GET',
    params: { cap_token }
  })
}

/**
 * 轮询 OTP 状态
 * @param {string} session_token - OTP session token
 */
export function pollOTP(session_token) {
  return request({
    url: '/api/login',
    method: 'GET',
    params: { session_token }
  })
}

/**
 * 获取用户信息
 * @param {string} access_token - JWT token
 */
export function getUserData(access_token) {
  return request({
    url: '/api/userdata',
    method: 'POST',
    data: { access_token }
  })
}

// ==================== 机器人管理 ====================

/**
 * 创建机器人
 * @param {string} username - MC 用户名
 */
export function createBot(data) {
  return request({
    url: '/api/createbot',
    method: 'POST',
    data
  })
}

/**
 * 获取我的机器人列表
 */
export function getMyBotsList() {
  return request({
    url: '/api/getmybotslist',
    method: 'POST',
    data: {}
  })
}

/**
 * 验证机器人
 * @param {string} username - MC 用户名
 * @param {string} code - 验证码
 */
export function verifyBot(data) {
  return request({
    url: '/api/verifybot',
    method: 'POST',
    data
  })
}

/**
 * 获取机器人验证码
 * @param {string} username - MC 用户名
 */
export function getVerifyCode(data) {
  return request({
    url: '/api/verifycode',
    method: 'POST',
    data
  })
}

/**
 * 获取机器人状态
 * @param {string} username - MC 用户名
 */
export function getBotStatus(data) {
  return request({
    url: '/api/getbotstatus',
    method: 'POST',
    data
  })
}

/**
 * 更新机器人配置
 */
export function updateBotConfig(data) {
  return request({
    url: '/api/updatebotconfig',
    method: 'POST',
    data
  })
}

/**
 * 发送信息给机器人
 */
export function sendInfo(data) {
  return request({
    url: '/api/sendinfo',
    method: 'POST',
    data
  })
}

/**
 * 获取配置
 */
export function getConfig() {
  return request({
    url: '/api/config',
    method: 'GET'
  })
}

// ==================== WebSocket 连接 ====================

/**
 * 创建 WebSocket 连接
 * @param {string} endpoint - WebSocket 端点 (如 '/ws/api/startbot')
 * @param {Function} onMessage - 消息回调
 * @param {Function} onError - 错误回调
 * @param {Function} onClose - 关闭回调
 */
export function createWebSocket(endpoint, { onMessage, onError, onClose } = {}) {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const host = import.meta.env.VITE_WS_BASE_URL || window.location.host
  const wsUrl = `${protocol}//${host}${endpoint}`
  
  const ws = new WebSocket(wsUrl)
  
  ws.onopen = () => {
    console.log('WebSocket 连接成功:', endpoint)
  }
  
  ws.onmessage = (event) => {
    if (onMessage) {
      try {
        const data = JSON.parse(event.data)
        onMessage(data)
      } catch (e) {
        onMessage(event.data)
      }
    }
  }
  
  ws.onerror = (error) => {
    console.error('WebSocket 错误:', error)
    if (onError) onError(error)
  }
  
  ws.onclose = (event) => {
    console.log('WebSocket 已关闭:', event.code, event.reason)
    if (onClose) onClose(event)
  }
  
  return ws
}

export default {
  // 认证
  login,
  authWithFanVerify,
  generateOTP,
  pollOTP,
  getUserData,
  
  // 机器人
  createBot,
  getMyBotsList,
  verifyBot,
  getVerifyCode,
  getBotStatus,
  updateBotConfig,
  sendInfo,
  getConfig,
  
  // WebSocket
  createWebSocket
}
