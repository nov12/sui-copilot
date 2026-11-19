<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { copyTextToClipboard } from './clipboard'

const props = defineProps<{
  examples: { label: string; text: string }[]
  copyLabel: string
  selectLabel?: string
}>()

const selected = ref(0)
const text = computed(() => props.examples[selected.value]?.text ?? '')
const feedback = ref('')

watch(text, () => { feedback.value = '' })

async function copy() {
  const value = text.value
  const copied = await copyTextToClipboard(value)
  if (value === text.value) feedback.value = copied ? '已复制' : '复制失败，可以手动选择内容复制。'
}
</script>

<template>
  <div class="code-example">
    <div class="example-controls">
      <select v-if="examples.length > 1" v-model="selected" :aria-label="selectLabel || '示例格式'">
        <option v-for="(example, index) in examples" :key="example.label" :value="index">{{ example.label }}</option>
      </select>
      <span v-else class="field-hint">{{ examples[0]?.label }}</span>
      <button type="button" class="secondary compact-action" @click="copy">{{ copyLabel }}</button>
    </div>
    <pre tabindex="0"><code>{{ text }}</code></pre>
    <p v-if="feedback" class="field-hint copy-feedback" role="status">{{ feedback }}</p>
  </div>
</template>

<style scoped>
.code-example { min-width: 0; padding: 12px; border-radius: 10px; background: #0f172a; }
.example-controls { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 8px; }
.example-controls select { width: auto; min-height: 34px; padding: 5px 8px; font-size: .8rem; }
.code-example pre { margin: 8px 0 0; overflow: auto; font: .8rem/1.55 ui-monospace, SFMono-Regular, Menlo, monospace; }
.copy-feedback { margin-top: 6px; }
</style>
