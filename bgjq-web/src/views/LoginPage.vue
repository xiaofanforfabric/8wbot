<template>
  <div class="nes-main-container">
    <div class="login-pixel-wrapper">
      <div class="nes-container with-title is-rounded nes-custom-card login-pixel-box">
        <p class="title card-title">8W 社区 · 身份认证</p>

        <!-- 像素风格 Tab 切换 -->
        <div class="pixel-tab-group">
          <button 
            type="button" 
            class="nes-btn" 
            :class="activeTab === 'code' ? 'is-primary' : ''"
            @click="activeTab = 'code'"
          >
            通行安全码
          </button>
          <button 
            type="button" 
            class="nes-btn" 
            :class="activeTab === 'otp' ? 'is-primary' : ''"
            @click="switchToOtp"
          >
            扫码快捷登录
          </button>
        </div>

        <!-- 提示框 -->
        <div v-if="msg.text" :class="['nes-alert', msg.type]">
          <span>{{ msg.text }}</span>
        </div>

        <!-- Tab 1: 动态验证码登录 -->
        <div v-if="activeTab === 'code'" class="tab-body">
          <div class="nes-field-wrap">
            <label for="uid_field">通行账号 UID</label>
            <input 
              id="uid_field" 
              v-model.trim="formData.user_id" 
              type="text" 
              inputmode="numeric" 
              class="nes-input" 
              placeholder="输入你的 UID"
              @input="msg.text = ''"
            />
          </div>

          <div class="nes-field-wrap">
            <label for="code_field">动态安全码 (Pass Code)</label>
            <input 
              id="code_field" 
              v-model.trim="formData.verify_code" 
              type="text" 
              class="nes-input" 
              placeholder="输入 6 位动态通行码"
              @input="msg.text = ''"
              @keyup.enter="handleLogin"
            />
          </div>

          <!-- Cap 验证码容器 -->
          <div class="cap-pixel-box">
            <div id="main-cap-container"></div>
          </div>

          <!-- 登录按钮：只要填了 UID 和验证码就能点击，如果 Cap 未加载完点击时会主动触发提示并尝试重试 -->
          <button 
            type="button" 
            class="nes-btn is-success full-width-btn" 
            :class="{ 'is-disabled': loading || !formData.user_id || !formData.verify_code }"
            :disabled="loading"
            @click="handleLogin"
          >
            {{ loading ? '正在验证...' : '确认安全登录' }}
          </button>
        </div>

        <!-- Tab 2: OTP 扫码模式 -->
        <div v-if="activeTab === 'otp'" class="tab-body text-center">
          <div v-if="otpState.step === 'captcha'">
            <p style="margin-bottom: 14px; font-weight: bold;">请先完成真人验证生成扫码凭据</p>
            <div id="otp-cap-container" class="cap-pixel-box"></div>
            <button type="button" class="nes-btn is-warning" style="margin-top: 14px;" @click="generateQR">
              {{ otpState.capToken ? '生成扫码链接' : '完成验证后点击生成' }}
            </button>
          </div>

          <div v-if="otpState.step === 'qr'">
            <a :href="otpState.qrUrl" target="_blank" rel="noopener" class="nes-btn is-primary full-width-btn">
              打开扫码授权页面 ↗
            </a>
            <p style="margin-top: 16px; font-weight: bold;">
              <span v-if="otpState.status === 'waiting'">等待扫码确认 (剩余 {{ otpState.countdown }}s)</span>
              <span v-else-if="otpState.status === 'success'" style="color: var(--nes-green);">授权成功，正在载入...</span>
              <span v-else-if="otpState.status === 'expired'" style="color: var(--nes-red);">凭据已过期，请重试</span>
            </p>
          </div>
        </div>

        <div style="margin-top: 20px; text-align: center;">
          <router-link to="/" class="nes-btn">返回主页</router-link>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import { useUserStore } from '../stores/user'
import { login, generateOTP, pollOTP } from '../api'

const router = useRouter()
const userStore = useUserStore()

const activeTab = ref('code')
const formData = reactive({ user_id: '', verify_code: '' })
const captchaToken = ref('')
const loading = ref(false)
const msg = reactive({ text: '', type: '' })

const otpState = reactive({
  step: 'captcha',
  capToken: '',
  sessionToken: '',
  qrUrl: '',
  status: '',
  countdown: 0,
  manualReady: true
})

let mainCapSolver = null
let otpCapSolver = null
let pollTimer = null
let countTimer = null

onMounted(() => {
  initMainCap()
})

function initMainCap() {
  if (window.CapSolver) {
    try {
      mainCapSolver = new window.CapSolver('main-cap-container', {
        onSuccess: (tok) => {
          captchaToken.value = tok
        },
        onError: (err) => {
          console.warn('CapSolver callback error:', err)
        }
      })
    } catch (e) {
      console.error('CapSolver init error:', e)
    }
  } else {
    setTimeout(initMainCap, 300)
  }
}

function switchToOtp() {
  activeTab.value = 'otp'
  otpState.step = 'captcha'
  nextTick(() => {
    if (window.CapSolver) {
      try {
        otpCapSolver = new window.CapSolver('otp-cap-container', {
          onSuccess: (tok) => {
            otpState.capToken = tok
            generateQR()
          }
        })
      } catch (e) {}
    }
  })
}

async function handleLogin() {
  if (!formData.user_id) {
    msg.text = '请输入你的通行账号 UID'
    msg.type = 'error'
    return
  }
  if (!formData.verify_code) {
    msg.text = '请输入动态通行码'
    msg.type = 'error'
    return
  }
  if (!captchaToken.value) {
    msg.text = '请先点击下方人机验证框完成验证'
    msg.type = 'error'
    return
  }

  loading.value = true
  msg.text = ''

  try {
    const res = await login({
      user_id: formData.user_id,
      verify_code: formData.verify_code,
      cap_token: captchaToken.value
    })

    if (res.code === 200 || res.code === '200' || res.status === 'ok') {
      msg.text = '认证成功，正在载入...'
      msg.type = 'success'
      const tk = res.accesstoken || res.token
      userStore.setToken(tk)
      setTimeout(() => router.push('/user'), 600)
    } else {
      msg.text = res.msg || res.message || '登录失败，请检查通行码是否过期'
      msg.type = 'error'
      captchaToken.value = ''
      if (mainCapSolver && mainCapSolver.reset) mainCapSolver.reset()
    }
  } catch (err) {
    msg.text = err.message || '网络连接异常，请稍后再试'
    msg.type = 'error'
    captchaToken.value = ''
    if (mainCapSolver && mainCapSolver.reset) mainCapSolver.reset()
  } finally {
    loading.value = false
  }
}

async function generateQR() {
  const cap = otpState.capToken || captchaToken.value
  if (!cap) {
    msg.text = '请先完成人机验证'
    msg.type = 'error'
    return
  }

  try {
    const res = await generateOTP(cap)
    if (!res.qr_coder) {
      msg.text = res.msg || '获取扫码凭据失败'
      msg.type = 'error'
      return
    }
    otpState.sessionToken = res.session_token
    otpState.qrUrl = res.qr_coder
    otpState.step = 'qr'
    otpState.status = 'waiting'
    otpState.countdown = 120

    clearInterval(countTimer)
    countTimer = setInterval(() => {
      otpState.countdown--
      if (otpState.countdown <= 0) {
        otpState.status = 'expired'
        clearInterval(pollTimer)
        clearInterval(countTimer)
      }
    }, 1000)

    startPoll()
  } catch (e) {
    msg.text = e.message || '生成失败'
    msg.type = 'error'
  }
}

function startPoll() {
  clearInterval(pollTimer)
  pollTimer = setInterval(async () => {
    if (otpState.status !== 'waiting') return
    try {
      const res = await pollOTP(otpState.sessionToken)
      if (res.status === 'ok') {
        otpState.status = 'success'
        clearInterval(pollTimer)
        clearInterval(countTimer)
        userStore.setToken(res.access_token)
        setTimeout(() => router.push('/user'), 800)
      } else if (res.status === 'expired') {
        otpState.status = 'expired'
        clearInterval(pollTimer)
      }
    } catch (_) {}
  }, 4000)
}
</script>

<style scoped>
.login-pixel-wrapper {
  max-width: 480px;
  margin: 20px auto;
}

.login-pixel-box {
  background-color: var(--nes-bg-card);
}

.pixel-tab-group {
  display: flex;
  gap: 10px;
  margin-bottom: 20px;
}

.pixel-tab-group button {
  flex: 1;
}

.cap-pixel-box {
  min-height: 60px;
  display: flex;
  justify-content: center;
  align-items: center;
  margin: 14px 0 18px;
}

.full-width-btn {
  width: 100%;
}

.text-center {
  text-align: center;
}
</style>
