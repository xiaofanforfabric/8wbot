<template>
  <div class="nes-main-container">
    <div class="create-pixel-wrapper">
      <div class="nes-container with-title is-rounded nes-custom-card">
        <p class="title card-title">新建挂机假人</p>

        <div v-if="msg.text" :class="['nes-alert', msg.type]">
          <span>{{ msg.text }}</span>
        </div>

        <div class="nes-field-wrap">
          <label for="bot_name_field">游戏角色名 (Minecraft ID)</label>
          <input 
            id="bot_name_field" 
            v-model.trim="username" 
            type="text" 
            class="nes-input" 
            placeholder="例如: 8W_Bot_01"
            @input="msg.text = ''"
            @keyup.enter="handleCreate"
          />
        </div>

        <div class="nes-container is-rounded" style="padding: 12px; margin-bottom: 20px; font-size: 0.85rem; background-color: var(--nes-bg);">
          <p>★ 提示：创建成功后将自动分配运行节点，可在控制台随时上线至 bgjq.simpfun.cn 服务器。</p>
        </div>

        <div class="create-actions">
          <button 
            type="button" 
            class="nes-btn is-primary full-btn" 
            :disabled="loading || !username.trim()"
            @click="handleCreate"
          >
            {{ loading ? '正在初始化节点...' : '确认创建假人' }}
          </button>
          <router-link to="/user" class="nes-btn full-btn">返回控制台</router-link>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive } from 'vue'
import { useRouter } from 'vue-router'
import { createBot } from '../api'

const router = useRouter()
const username = ref('')
const loading = ref(false)
const msg = reactive({ text: '', type: '' })

async function handleCreate() {
  if (!username.value.trim()) return
  loading.value = true
  msg.text = ''

  try {
    const res = await createBot({ username: username.value.trim() })
    if (res.code === 200 || res.code === '200' || res.status === 'ok') {
      msg.text = '创建成功，正在返回列表...'
      msg.type = 'success'
      setTimeout(() => router.push('/user'), 700)
    } else {
      msg.text = res.msg || res.message || '创建失败，可能名称已存在或配额不足'
      msg.type = 'error'
    }
  } catch (err) {
    msg.text = err.message || '网络通讯异常'
    msg.type = 'error'
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.create-pixel-wrapper {
  max-width: 520px;
  margin: 30px auto;
}

.create-actions {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.full-btn {
  width: 100%;
}
</style>
