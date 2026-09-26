<template>
  <div class="nes-main-container">
    <div class="console-nes-wrapper">
      
      <!-- 控制台头部工具栏 -->
      <div class="nes-container with-title is-rounded nes-custom-card" style="margin-bottom: 20px;">
        <p class="title card-title">{{ botname }} · 实时控制面板</p>
        
        <div class="console-head-flex">
          <div class="console-status-group">
            <span :class="['nes-tag-status', wsConnected ? 'online' : 'offline']">
              {{ wsConnected ? '● 数据流正常' : '○ 通道已断开' }}
            </span>
          </div>

          <div class="console-btn-group">
            <button type="button" class="nes-btn is-warning" @click="clearLogs">清屏</button>
            <button type="button" class="nes-btn is-primary" @click="reconnect">重连通道</button>
            <router-link to="/user" class="nes-btn">返回列表</router-link>
          </div>
        </div>
      </div>

      <!-- 像素终端屏幕 -->
      <div class="nes-container is-dark is-rounded pixel-terminal-screen">
        <div class="terminal-logs-content" ref="logBox">
          <div v-if="logs.length === 0" style="color: #888; text-align: center; padding: 40px 0;">
            [ 正在监听游戏数据流与指令交互... ]
          </div>
          <div v-for="(item, idx) in logs" :key="idx" class="terminal-row">
            <span class="t-time">[{{ item.time }}]</span>
            <span :class="['t-msg', item.type]">{{ item.text }}</span>
          </div>
        </div>

        <!-- 命令行输入栏 -->
        <div class="terminal-cmd-bar">
          <span style="color: var(--nes-green); font-weight: bold; margin-right: 6px;">&gt;</span>
          <input 
            v-model="cmdInput" 
            type="text" 
            class="nes-input is-dark cmd-pixel-input" 
            placeholder="输入发送给服务器的聊天或指令 (如 /help)..."
            @keyup.enter="handleSend"
          />
          <button type="button" class="nes-btn is-success" :disabled="!cmdInput.trim()" @click="handleSend">
            发送
          </button>
        </div>
      </div>

    </div>
  </div>
</template>

<script setup>
import { ref, onMounted, onUnmounted, nextTick } from 'vue'
import { useRoute } from 'vue-router'
import { createWebSocket, sendInfo } from '../api'

const route = useRoute()
const botname = ref(route.params.botname || 'default')

const logs = ref([])
const cmdInput = ref('')
const wsConnected = ref(false)
const logBox = ref(null)
let ws = null

onMounted(() => {
  connect()
})

onUnmounted(() => {
  if (ws) ws.close()
})

function connect() {
  if (ws) ws.close()
  
  ws = createWebSocket(`/ws/api/connectbot?username=${botname.value}`, {
    onMessage: (data) => {
      wsConnected.value = true
      let parsed = typeof data === 'string' ? data : JSON.stringify(data)
      appendLog(parsed, 'info')
    },
    onError: () => {
      wsConnected.value = false
      appendLog('连接控制台失败，假人可能处于休眠状态', 'error')
    },
    onClose: () => {
      wsConnected.value = false
    }
  })
}

function reconnect() {
  appendLog('正在重新建立数据通道...', 'warn')
  connect()
}

function appendLog(text, type = 'normal') {
  const d = new Date()
  const time = `${d.getHours().toString().padStart(2,'0')}:${d.getMinutes().toString().padStart(2,'0')}:${d.getSeconds().toString().padStart(2,'0')}`
  
  logs.value.push({ time, text, type })
  if (logs.value.length > 400) logs.value.shift()

  nextTick(() => {
    if (logBox.value) {
      logBox.value.scrollTop = logBox.value.scrollHeight
    }
  })
}

async function handleSend() {
  if (!cmdInput.value.trim()) return
  const msg = cmdInput.value.trim()
  cmdInput.value = ''

  appendLog(`[发出指令] ${msg}`, 'cmd')
  try {
    await sendInfo({
      username: botname.value,
      message: msg
    })
  } catch (err) {
    appendLog(`[发送失败] ${err.message || '未知异常'}`, 'error')
  }
}

function clearLogs() {
  logs.value = []
}
</script>

<style scoped>
.console-head-flex {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
}

.console-btn-group {
  display: flex;
  gap: 8px;
}

.pixel-terminal-screen {
  background-color: #000 !important;
  color: #fff;
  display: flex;
  flex-direction: column;
  height: 520px;
  padding: 15px !important;
}

.terminal-logs-content {
  flex: 1;
  overflow-y: auto;
  font-family: var(--font-mono);
  font-size: 0.86rem;
  line-height: 1.6;
  padding-bottom: 12px;
}

.terminal-row {
  word-break: break-all;
  margin-bottom: 4px;
}

.t-time {
  color: #777;
  margin-right: 6px;
}

.t-msg.normal { color: #eee; }
.t-msg.info { color: #209cee; }
.t-msg.warn { color: #f7d51d; }
.t-msg.error { color: #e76e55; }
.t-msg.cmd { color: #92cc41; font-weight: bold; }

.terminal-cmd-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  border-top: 2px solid #333;
  padding-top: 10px;
}

.cmd-pixel-input {
  flex: 1;
  font-family: var(--font-mono);
}
</style>
