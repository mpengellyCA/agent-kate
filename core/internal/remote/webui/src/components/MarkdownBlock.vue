<script setup>
import { computed } from 'vue'
import { renderMarkdown } from '../lib/render.js'

const props = defineProps({ source: { type: String, default: '' } })
// This is the only v-html binding in the web client. renderMarkdown is covered
// by the sanitizer corpus and rejects remote images, scripts, and unsafe URLs.
const html = computed(() => renderMarkdown(props.source))
</script>

<template><div class="markdown" v-html="html" /></template>
