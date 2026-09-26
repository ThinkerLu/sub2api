<template>
  <div class="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2" data-testid="responses-ws-profile-fields">
    <div>
      <label class="input-label">{{ t('admin.accounts.openai.wsProfile') }}</label>
      <Select v-model="profileModel" :options="options" data-testid="responses-ws-profile-select" />
    </div>
    <div v-if="profile">
      <label class="input-label">{{ t('admin.accounts.openai.wsEndpoint') }}</label>
      <input v-model="endpointModel" type="url" class="input w-full" data-testid="responses-ws-endpoint" :aria-label="t('admin.accounts.openai.wsEndpoint')" placeholder="wss://api.example.com/responses" required />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import { RESPONSES_WS_PROFILE_V2 } from '@/utils/openaiWsMode'

const props = defineProps<{ profile: string; endpoint: string }>()
const emit = defineEmits<{ 'update:profile': [value: string]; 'update:endpoint': [value: string] }>()
const { t } = useI18n()
const profileModel = computed({ get: () => props.profile, set: value => emit('update:profile', value) })
const endpointModel = computed({ get: () => props.endpoint, set: value => emit('update:endpoint', value) })
const options = computed(() => [
  { value: '', label: t('admin.accounts.openai.wsProfileNone') },
  { value: RESPONSES_WS_PROFILE_V2, label: 'Responses WS v2 (Bearer)' }
])
</script>
