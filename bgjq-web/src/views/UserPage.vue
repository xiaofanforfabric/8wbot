<template>
  <div class="nes-main-container">
    <!-- 用户概况卡片 -->
    <div class="nes-container with-title is-rounded nes-custom-card">
      <p class="title card-title">用户信息看板</p>
      
      <div class="profile-nes-grid">
        <div class="user-meta">
          <p><strong>账号 UID：</strong><span>{{ userStore.user?.jht_uid || '8W-User' }}</span></p>
          <p><strong>权限等级：</strong><span class="nes-tag-status online">Lv.{{ userStore.user?.level || '1' }} 社区成员</span></p>
          <p><strong>挂机配额：</strong><span>{{ userStore.user?.remaining_bot_creation_quantity ?? 1 }} 个</span></p>
        </div>

        <div class="user-quick-actions">
          <router-link to="/create-bot" class="nes-btn is-primary">+ 新建假人</router-link>
          <button type="button" class="nes-btn is-warning" @click="fetchBots">刷新列表</button>
        </div>
      </div>
    </div>

    <!-- 挂机机器人列表 -->
    <div class="nes-section-banner" style="font-size: 1.1rem; padding: 10px;">
      ★ 挂机假人节点状态 ★
    </div>

    <div v-if="loading" class="nes-container is-rounded" style="text-align: center; padding: 30px;">
      <p>正在同步挂机数据中...</p>
    </div>

    <div v-else-if="bots.length === 0" class="nes-container is-rounded" style="text-align: center; padding: 40px;">
      <p style="font-weight: bold; margin-bottom: 10px;">当前暂无运行中的假人</p>
      <router-link to="/create-bot" class="nes-btn is-success">立即创建首个假人</router-link>
    </div>

    <div v-else class="bot-pixel-grid">
      <div v-for="bot in bots" :key="bot.username" class="nes-container with-title is-rounded nes-custom-card bot-pixel-card">
        <p class="title card-title">{{ bot.username }}</p>

        <div class="bot-meta-line">
          <span>运行状态：</span>
          <span :class="['nes-tag-status', bot.status === 'online' ? 'online' : 'offline']">
            {{ bot.status === 'online' ? '● 挂机中' : '○ 已休眠' }}
          </span>
        </div>

        <div class="bot-meta-line">
          <span>自动重连：</span>
          <strong>{{ bot.auto_reconnect ? '开启' : '关闭' }}</strong>
        </div>

        <div class="bot-meta-line">
          <span>创建时间：</span>
          <span style="font-size: 0.8rem;">{{ formatDate(bot.creation_time) }}</span>
        </div>

        <div class="bot-card-buttons">
          <router-link :to="`/console/${bot.username}`" class="nes-btn is-primary">控制台</router-link>
          <button 
            type="button" 
            class="nes-btn" 
            :class="bot.status === 'online' ? 'is-error' : 'is-success'"
            :disabled="actionLoading[bot.username]"
            @click="toggleBotStatus(bot)"
          >
            {{ bot.status === 'online' ? '下线' : '上线' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { useUserStore } from '../stores/user'
import { getMyBotsList, createWebSocket } from '../api'

const userStore = useUserStore()
const bots = ref([])
const loading = ref(true)
const actionLoading = reactive({})

onMounted(async () => {
  await Promise.all([
    userStore.fetchUserInfo(),
    fetchBots()
  ])
})

async function fetchBots() {
  loading.value = true
  try {
    const res = await getMyBotsList()
    bots.value = res.bots || []
  } catch (e) {
    console.error('获取列表失败:', e)
  } finally {
    loading.value = false
  }
}

function toggleBotStatus(bot) {
  const isOnline = bot.status === 'online'
  const endpoint = isOnline ? `/ws/api/stopbot` : `/ws/api/startbot`
  
  actionLoading[bot.username] = true
  const ws = createWebSocket(endpoint, {
    onMessage: () => {
      bot.status = isOnline ? 'offline' : 'online'
      actionLoading[bot.username] = false
      ws.close()
    },
    onError: () => {
      actionLoading[bot.username] = false
    }
  })
}

function formatDate(d) {
  if (!d) return '--'
  return String(d).split(' ')[0] || d
}
</script>

<style scoped>
.profile-nes-grid {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 15px;
}

.user-meta p {
  margin-bottom: 6px;
}

.user-quick-actions {
  display: flex;
  gap: 10px;
}

.bot-pixel-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 20px;
}

.bot-pixel-card {
  padding-top: 24px;
}

.bot-meta-line {
  display: flex;
  justify-content: space-between;
  margin-bottom: 8px;
  font-size: 0.9rem;
}

.bot-card-buttons {
  display: flex;
  gap: 10px;
  margin-top: 16px;
}

.bot-card-buttons .nes-btn {
  flex: 1;
}
</style>
