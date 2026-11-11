import { createApp } from 'vue';
import App from '@/App.vue';
import vuetify from '@/plugins/vuetify';
import { i18n } from '@/i18n';

createApp(App).use(vuetify).use(i18n).mount('#app');
