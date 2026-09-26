<template>
  <div class="nes-header-wrapper">
    <!-- 8W 官网同款蓝色大顶栏 -->
    <header class="nes-top-header">
      <div class="header-container">
        <router-link to="/" class="header-logo-group">
          <img src="https://8w.bgjq.top/images/logo.webp" alt="8W社区Logo" class="logo-pixel" />
          <div class="header-titles">
            <h1 class="logo-title">8W 社区</h1>
            <p class="logo-sub">挂机平台 · 8w Community</p>
          </div>
        </router-link>

        <div class="header-actions">
          <button 
            type="button" 
            class="nes-btn" 
            :class="isDark ? 'is-warning' : 'is-dark'"
            @click="toggleTheme"
          >
            {{ isDark ? '☀ 浅色' : '🌙 深色' }}
          </button>

          <template v-if="userStore.token">
            <router-link to="/user" class="nes-btn is-primary">控制台</router-link>
            <button type="button" class="nes-btn is-error" @click="userStore.logout">退出</button>
          </template>
          <template v-else>
            <router-link to="/login" class="nes-btn is-primary">登录</router-link>
          </template>
        </div>
      </div>
    </header>

    <!-- 8W 官网同款黑色像素横条菜单 -->
    <nav class="nes-navbar-strip">
      <div class="nav-container">
        <ul class="nes-nav-menu">
          <li><router-link to="/" class="nes-nav-link" active-class="active" exact>首页</router-link></li>
          <li v-if="userStore.token"><router-link to="/user" class="nes-nav-link" active-class="active">挂机管理</router-link></li>
          <li v-if="userStore.token"><router-link to="/create-bot" class="nes-nav-link" active-class="active">新建假人</router-link></li>
          <li><a href="https://8w.bgjq.top/" target="_blank" rel="noopener" class="nes-nav-link">社区官网 ↗</a></li>
          <li><a href="https://qm.qq.com/q/hELXutcWZy" target="_blank" rel="noopener" class="nes-nav-link">QQ 群</a></li>
        </ul>
      </div>
    </nav>
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { useUserStore } from '../stores/user'

const userStore = useUserStore()
const isDark = ref(false)

onMounted(() => {
  const saved = localStorage.getItem('8w_nes_theme')
  if (saved === 'dark') {
    isDark.value = true
    document.documentElement.classList.add('dark-theme')
  } else {
    isDark.value = false
    document.documentElement.classList.remove('dark-theme')
  }
})

function toggleTheme() {
  isDark.value = !isDark.value
  if (isDark.value) {
    document.documentElement.classList.add('dark-theme')
    localStorage.setItem('8w_nes_theme', 'dark')
  } else {
    document.documentElement.classList.remove('dark-theme')
    localStorage.setItem('8w_nes_theme', 'light')
  }
}
</script>

<style scoped>
.nes-top-header {
  background-color: var(--nes-header-bg);
  color: #fff;
  padding: 20px 0;
  border-bottom: 4px solid var(--nes-dark);
  box-shadow: 0 4px 0 rgba(0, 0, 0, 0.2);
}

.header-container {
  max-width: 1100px;
  margin: 0 auto;
  padding: 0 20px;
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 15px;
}

.header-logo-group {
  display: flex;
  align-items: center;
  gap: 16px;
  text-decoration: none;
  color: #fff;
}

.logo-pixel {
  width: 58px;
  height: 58px;
  border: 3px solid #fff;
  box-shadow: 3px 3px 0 var(--nes-dark);
  image-rendering: pixelated;
  background: #fff;
}

.logo-title {
  font-family: var(--font-pixel);
  font-size: 1.4rem;
  letter-spacing: 1px;
  text-shadow: 2px 2px 0 var(--nes-dark);
  margin-bottom: 2px;
}

.logo-sub {
  font-size: 0.82rem;
  opacity: 0.95;
  text-shadow: 1px 1px 0 rgba(0, 0, 0, 0.4);
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 10px;
}

/* 黑色像素导航菜单 */
.nes-navbar-strip {
  background-color: var(--nes-nav-bg);
  border-bottom: 4px solid var(--nes-dark);
  position: sticky;
  top: 0;
  z-index: 1000;
}

.nav-container {
  max-width: 1100px;
  margin: 0 auto;
  padding: 0 20px;
}

.nes-nav-menu {
  list-style: none;
  display: flex;
  flex-wrap: wrap;
}

.nes-nav-link {
  display: block;
  padding: 10px 18px;
  color: #fff;
  text-decoration: none;
  font-size: 0.88rem;
  font-weight: 700;
  border-right: 2px solid var(--nes-dark);
  transition: all 0.1s;
}

.nes-nav-link:hover,
.nes-nav-link.active {
  background-color: var(--nes-blue);
  color: #fff;
}
</style>
