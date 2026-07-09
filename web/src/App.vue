<script setup lang="ts">
import { computed, defineAsyncComponent, ref, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { useAuthStore } from './stores/auth'
import LoadingScreen from './components/LoadingScreen.vue'

const route = useRoute()
const auth = useAuthStore()

const isDark = ref(localStorage.getItem('theme') === 'dark')

function toggleTheme() {
  isDark.value = !isDark.value
  const mode = isDark.value ? 'dark' : 'light'
  localStorage.setItem('theme', mode)
  updateHtmlClass(mode)
}

function updateHtmlClass(mode: 'dark' | 'light') {
  if (mode === 'dark') {
    document.documentElement.classList.add('dark')
  } else {
    document.documentElement.classList.remove('dark')
  }
}

onMounted(() => {
  if (isDark.value) {
    updateHtmlClass('dark')
  }
})

const AuthenticatedShell = defineAsyncComponent(() => import('./layouts/AuthenticatedShell.vue'))
const UnauthenticatedShell = defineAsyncComponent(() => import('./layouts/UnauthenticatedShell.vue'))
const shell = computed(() =>
  auth.isAuthenticated && route.name !== 'Login' ? AuthenticatedShell : UnauthenticatedShell
)
</script>

<template>
  <div class="h-screen w-screen overflow-hidden bg-gray-50 dark:bg-[#101014] text-gray-900 dark:text-gray-100 font-sans selection:bg-indigo-500 selection:text-white transition-colors duration-300">
    <Suspense>
      <template #default>
        <component :is="shell" :is-dark="isDark" @toggle-theme="toggleTheme" />
      </template>
      <template #fallback>
        <LoadingScreen />
      </template>
    </Suspense>

  </div>
</template>

<style>
/* Custom Scrollbar */
::-webkit-scrollbar {
  width: 8px;
  height: 8px;
}
::-webkit-scrollbar-track {
  background: transparent;
}
::-webkit-scrollbar-thumb {
  background: #cbd5e1;
  border-radius: 4px;
}
.dark ::-webkit-scrollbar-thumb {
  background: #334155;
}
::-webkit-scrollbar-thumb:hover {
  background: #94a3b8;
}
.dark ::-webkit-scrollbar-thumb:hover {
  background: #475569;
}
</style>
