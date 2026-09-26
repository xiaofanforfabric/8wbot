<template>
  <div class="nes-main-container">
    <!-- 8W 专属黄色像素大横幅 -->
    <div class="nes-section-banner">
      ★ 欢迎来到 8W 社区 · 假人挂机管理平台 ★
    </div>

    <!-- 关于平台（复刻 8W 官网 NES 容器） -->
    <div class="nes-container with-title is-rounded nes-custom-card">
      <p class="title card-title">平台简介</p>
      <div class="nes-content-body">
        <p>8W 社区挂机平台是专为邦国崛起玩家社区打造的自动化假人托管服务。支持多实例隔离运行、全天候在线保活与 WebSocket 实时控制台。</p>
        <p style="margin-top: 10px;">
          目标服务器：<strong style="color: var(--nes-blue);">bgjq.simpfun.cn</strong> · 欢迎加入我们的大家庭！
        </p>
        <div style="margin-top: 18px; display: flex; gap: 12px; flex-wrap: wrap;">
          <router-link to="/user" class="nes-btn is-primary">进入控制台</router-link>
          <router-link to="/login" class="nes-btn is-success">通行码登录</router-link>
          <a href="https://8w.bgjq.top/" target="_blank" class="nes-btn">社区主站 ↗</a>
        </div>
      </div>
    </div>

    <!-- 四大特色模块 -->
    <div class="nes-grid-cards">
      <div class="nes-container is-rounded feature-pixel-card">
        <h3 class="pixel-subtitle" style="color: var(--nes-blue);">🎮 稳定假人挂机</h3>
        <p>基于原生协议栈驱动，独立账号隔离运行，断线自动重连保活。</p>
      </div>

      <div class="nes-container is-rounded feature-pixel-card">
        <h3 class="pixel-subtitle" style="color: var(--nes-green);">🛡️ 安全通行认证</h3>
        <p>内置 Cap 真人验证与动态通行安全码机制，防护严密。</p>
      </div>

      <div class="nes-container is-rounded feature-pixel-card">
        <h3 class="pixel-subtitle" style="color: var(--nes-yellow);">⚡ 实时控制终端</h3>
        <p>全双工数据通道，聊天记录与服务器消息毫秒级流式回显。</p>
      </div>

      <div class="nes-container is-rounded feature-pixel-card">
        <h3 class="pixel-subtitle" style="color: var(--nes-red);">🕹️ 经典 NES 像素风</h3>
        <p>1:1 深度复刻 8W 社区官方复古街机 UI，支持深浅模式自由切换。</p>
      </div>
    </div>

    <!-- 底部社区信息 -->
    <div class="nes-container is-rounded nes-custom-card" style="margin-top: 30px; text-align: center;">
      <p style="font-weight: bold; margin-bottom: 6px;">8W 社区 · 维护和平 · 促进发展 · 共建和谐</p>
      <p style="font-size: 0.85rem; color: var(--nes-text-muted);">
        QQ 群：1085806711 · 服务器地址：bgjq.simpfun.cn
      </p>
    </div>
  </div>
</template>

<script setup>
</script>

<style scoped>
.nes-grid-cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 20px;
  margin-top: 24px;
}

.feature-pixel-card {
  background-color: var(--nes-bg-card);
  padding: 18px;
}

.pixel-subtitle {
  font-size: 1rem;
  font-weight: 900;
  margin-bottom: 10px;
}

.feature-pixel-card p {
  font-size: 0.88rem;
  line-height: 1.6;
}
</style>
