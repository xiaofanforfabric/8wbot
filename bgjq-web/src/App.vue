<script setup>
import { onMounted } from 'vue'
import AppNavbar from './components/AppNavbar.vue'

// 动态注入 Cap 人机验证 Widget 脚本
onMounted(() => {
  if (!document.getElementById('cap-widget-script')) {
    const script = document.createElement('script')
    script.id = 'cap-widget-script'
    script.src = 'https://cap.fanverify.cn/ce31c7abec/widget.js'
    script.setAttribute('data-cap-auto-bind', 'false')
    document.head.appendChild(script)
  }
})
</script>

<template>
  <div class="app-layout">
    <AppNavbar />
    <router-view v-slot="{ Component }">
      <transition name="fade-slide" mode="out-in">
        <component :is="Component" />
      </transition>
    </router-view>
  </div>
</template>

<style>
.app-layout {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
}

/* 页面切换平滑淡入动效 */
.fade-slide-enter-active,
.fade-slide-leave-active {
  transition: opacity 0.2s cubic-bezier(0.16, 1, 0.3, 1), transform 0.2s cubic-bezier(0.16, 1, 0.3, 1);
}

.fade-slide-enter-from {
  opacity: 0;
  transform: translateY(8px);
}

.fade-slide-leave-to {
  opacity: 0;
  transform: translateY(-8px);
}
</style>
