<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { ApiError, api, type SuiLoginResult } from './api'

const emit = defineEmits<{ close: []; authenticated: [result: SuiLoginResult] }>()
const dialog = ref<HTMLDialogElement | null>(null)
const passwordInput = ref<HTMLInputElement | null>(null)
const account = ref('')
const password = ref('')
const busy = ref(false)
const errorMessage = ref('')

onMounted(() => dialog.value?.showModal())

function cancel(event: Event) {
  if (busy.value) event.preventDefault()
}

async function login() {
  if (busy.value || !account.value.trim() || !password.value) return
  busy.value = true
  errorMessage.value = ''
  try {
    const response = api.loginSui(account.value.trim(), password.value)
    password.value = ''
    emit('authenticated', await response)
  } catch (error) {
    errorMessage.value = error instanceof ApiError ? error.message : '暂时无法完成登录，请检查连接后重试'
  } finally {
    password.value = ''
    busy.value = false
  }
  if (errorMessage.value) {
    await nextTick()
    passwordInput.value?.focus()
  }
}
</script>

<template>
  <dialog ref="dialog" class="card sui-login-dialog" aria-labelledby="sui-login-title" @cancel="cancel" @close="emit('close')">
    <h2 id="sui-login-title">登录随手记</h2>
    <form method="post" @submit.prevent="login" :aria-busy="busy">
      <label>
        账号
        <input v-model="account" type="text" name="username" autocomplete="username" autocapitalize="none" :spellcheck="false" autofocus required :disabled="busy">
      </label>
      <label>
        密码
        <input ref="passwordInput" v-model="password" type="password" name="password" autocomplete="current-password" required :disabled="busy">
      </label>
      <p v-if="errorMessage" class="error" role="alert">{{ errorMessage }}</p>
      <div class="actions">
        <button type="button" class="secondary" :disabled="busy" @click="dialog?.close()">取消</button>
        <button type="submit" :disabled="busy || !account.trim() || !password">{{ busy ? '登录中…' : '登录' }}</button>
      </div>
    </form>
  </dialog>
</template>
