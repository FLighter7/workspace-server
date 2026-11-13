<style lang="scss" module>
.heading {
	margin-block: 2rem 0.75rem;
}

.apps {
	display: flex;
	flex-wrap: wrap;
	gap: 0.25rem 0.75rem;
	padding: 0;
	margin: 0;
	list-style: none;
}

// Each stopped app with its Resume button beside it.
.stopped {
	gap: 0.5rem 1.25rem;

	> li {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
}

.note {
	margin-block: 0.75rem 0;
}

.error {
	color: rgb(var(--v-theme-error));
}
</style>

<template>
	<section aria-labelledby="upstream-h">
		<h2 id="upstream-h" ref="heading" tabindex="-1" :class="['text-title-large', $style.heading]">
			{{ t('upstreamTitle') }}
		</h2>

		<template v-if="upstream?.linked">
			<FactList>
				<FactItem :label="t('upstreamMain')">
					<bdi>{{ upstream.name }}</bdi>
					{{ ' ' }}
					<code class="text-medium-emphasis">{{ upstream.serverId }}</code>
				</FactItem>
				<FactItem :label="t('upstreamState')">
					<StateBadge :tone="state.tone">{{ t(state.key) }}</StateBadge>
				</FactItem>
				<FactItem v-if="upstream.linkedAt" :label="t('linkedSince')">
					{{ format.time(upstream.linkedAt) }}
				</FactItem>
				<FactItem :label="t('lastSync')">
					{{ upstream.lastSync ? format.time(upstream.lastSync) : t('notYet') }}
				</FactItem>
				<FactItem :label="t('syncedApps')">
					<ul v-if="upstream.apps.length" :class="$style.apps">
						<li v-for="app in upstream.apps" :key="app">
							<code>{{ app }}</code>
						</li>
					</ul>
					<template v-else>{{ t('none') }}</template>
				</FactItem>
				<FactItem v-if="stopped.length" :label="t('stoppedApps')">
					<ul :class="[$style.apps, $style.stopped]">
						<li v-for="app in stopped" :key="app">
							<code>{{ app }}</code>
							<!-- A halted link syncs nothing until the server starts again. -->
							<VBtn
								v-if="!upstream.halted"
								size="small"
								:aria-label="t('resumeLabel', { app })"
								:loading="resuming === app"
								@click="resume(app)"
							>
								{{ t('resumeButton') }}
							</VBtn>
						</li>
					</ul>
				</FactItem>
				<FactItem :label="t('admittedDevices')">
					{{ format.number(upstream.devices ?? 0) }}
				</FactItem>
			</FactList>
			<p v-if="stopped.length" :class="$style.note">
				{{ t(upstream.halted ? 'stoppedAppsHaltedText' : 'stoppedAppsText') }}
			</p>
			<p v-if="upstream.halted" :class="[$style.note, $style.error]">
				{{ t(haltedKey(upstream.reason)) }}
			</p>
			<p v-else-if="lookingForMain" :class="$style.note">{{ t('mainSteppedDownText') }}</p>
			<p v-else-if="!upstream.connected" :class="$style.note">{{ t('connectingText') }}</p>

			<div :class="[s.actions, 'mt-4']">
				<!-- A halted link reaches no main, and only the main issues codes. -->
				<VBtn v-if="!upstream.halted" color="primary" variant="flat" @click="inviteOpen = true">
					{{ t('inviteButton') }}
				</VBtn>
				<VBtn :loading="promoting" @click="promote">{{ t('promoteButton') }}</VBtn>
				<VBtn @click="unlink">{{ t('unlinkButton') }}</VBtn>
			</div>
		</template>

		<template v-else-if="upstream">
			<template v-if="waiting.length">
				<p>{{ t('waitingText') }}</p>
				<ul :class="[$style.apps, 'mt-2']">
					<li v-for="server in waiting" :key="server.serverId">
						<bdi>{{ server.name }}</bdi>
					</li>
				</ul>
				<p :class="[$style.note, 'mb-3']">{{ t('waitingRemoveText') }}</p>
			</template>
			<p>{{ t('upstreamMainIntro') }}</p>
			<p v-if="upstream.subordinates > 0" :class="$style.note">
				{{ t('upstreamHasSubordinates') }}
			</p>
			<template v-else>
				<p class="text-medium-emphasis mt-3">{{ t('linkHow') }}</p>
				<p class="text-medium-emphasis mt-3">{{ t('linkDataNote') }}</p>
				<VForm :class="s.form" @submit.prevent="link(pasted)">
					<VTextarea
						v-model="pasted"
						:label="t('linkCodeLabel')"
						rows="3"
						auto-grow
						autocomplete="off"
						autocapitalize="none"
						spellcheck="false"
					/>
					<div :class="s.actions">
						<VBtn
							type="submit"
							color="primary"
							variant="flat"
							:loading="linking"
							:disabled="pasted.trim() === ''"
						>
							{{ t('linkButton') }}
						</VBtn>
						<VBtn :aria-pressed="scanning" @click="scanning = !scanning">
							{{ scanning ? t('scanStop') : t('scanButton') }}
						</VBtn>
					</div>
				</VForm>
				<CodeScanner v-if="scanning" @code="scanned" />
			</template>
		</template>

		<StatusLine :message="message" />

		<InviteDialog v-model="inviteOpen" :main="upstream?.name ?? ''" />
	</section>
</template>

<script setup lang="ts">
import { computed, ref, useTemplateRef } from 'vue';
import { useI18n } from 'vue-i18n';
import { ApiError, serverApi } from '@/api/client';
import type { UpstreamStatus } from '@/api/types';
import CodeScanner from '@/components/CodeScanner.vue';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import StateBadge from '@/components/StateBadge.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { haltedKey, resumeError, upstreamError } from '@/composables/upstream';
import { ask } from '@/composables/useConfirm';
import { useFormat } from '@/composables/useFormat';
import { invitedCode } from '@/composables/usePairingCode';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import type { MessageKey } from '@/i18n';
import s from '@/styles/section.module.scss';
import InviteDialog from './InviteDialog.vue';

/**
 * The link to an upstream server: this server's main, when it is a
 * subordinate. On a main, linking it by the main's pairing code, pasted or
 * scanned, and the servers it waits for after it took the role of main
 * over; on a subordinate, the link's state, resuming an app the write guard
 * stopped, pairing this computer's browser through the main, making this
 * server the main, and unlinking.
 */
const { t } = useI18n();
const format = useFormat();

const upstream = ref<UpstreamStatus | null>(null);
const message = ref<Message | null>(null);
const pasted = ref('');
const scanning = ref(false);
const linking = ref(false);
const promoting = ref(false);
/** The app whose Resume is in flight. */
const resuming = ref<string | null>(null);
const inviteOpen = ref(false);
const heading = useTemplateRef<HTMLElement>('heading');

const stopped = computed(() => upstream.value?.stopped ?? []);
const waiting = computed(() => upstream.value?.waiting ?? []);
/** The main stepped down to another one, which this server looks for. */
const lookingForMain = computed(
	() => !upstream.value?.connected && upstream.value?.reason === 'mainSteppedDown',
);

const state = computed((): { key: MessageKey; tone: 'on' | 'off' | 'error' } => {
	if (upstream.value?.halted) return { key: 'stateHalted', tone: 'error' };
	if (upstream.value?.connected) return { key: 'stateConnected', tone: 'on' };
	if (lookingForMain.value) return { key: 'stateLookingForMain', tone: 'off' };
	return { key: 'stateConnecting', tone: 'off' };
});

async function load(): Promise<void> {
	try {
		upstream.value = await serverApi.upstream();
	} catch (error) {
		message.value = errorMessage(error);
	}
}

usePolling(load);

async function link(code: string): Promise<void> {
	const text = code.trim();
	if (text === '' || linking.value) return;
	linking.value = true;
	scanning.value = false;
	message.value = info('linking');
	try {
		upstream.value = await serverApi.link(text);
	} catch (error) {
		message.value = upstreamError(error);
		return;
	} finally {
		linking.value = false;
	}

	pasted.value = '';
	message.value = { key: 'linked', values: { name: upstream.value.name ?? '' } };
	// The form went with the link: the focus stays in the section.
	await rescueFocus(heading.value);
}

/**
 * "Resume" beside an app the write guard stopped: it syncs with the main
 * again at once, or once the main is in reach, with a fresh count. No
 * question: nothing is lost, and a write loop that goes on stops it again.
 */
async function resume(app: string): Promise<void> {
	if (resuming.value !== null) return;
	resuming.value = app;
	try {
		upstream.value = await serverApi.resume(app);
		message.value = {
			key: upstream.value.connected ? 'resumed' : 'resumedLater',
			values: { app },
		};
	} catch (error) {
		// The list in step: the app may have been resumed meanwhile, by a
		// restart or on another tab.
		await load();
		message.value = resumeError(error);
	} finally {
		resuming.value = null;
	}
	// The button went with its row: the focus stays in the section.
	await rescueFocus(heading.value);
}

/** A code the camera saw: linked at once, and the camera stops. */
function scanned(code: string): void {
	if (!linking.value) void link(code);
}

/**
 * "Make this server the main": a planned switch while the main is
 * reachable, confirmed; when it is not, or turns out not to be, a forced
 * switch behind a confirmation that says what happens next.
 */
async function promote(): Promise<void> {
	const name = upstream.value?.name ?? '';
	const reachable = upstream.value?.connected === true && !upstream.value.halted;
	if (reachable) {
		if (!(await ask({ title: 'promoteTitle', text: 'promoteText', ok: 'promoteOk' }))) return;
		const outcome = await switchTo(false);
		if (outcome !== 'unreachable') return;
	}
	if (
		!(await ask({
			title: 'promoteForceTitle',
			name,
			text: 'promoteForceText',
			ok: 'promoteForceOk',
		}))
	) {
		return;
	}
	await switchTo(true);
}

/** Asks the server to become the main, and says how it went. */
async function switchTo(force: boolean): Promise<'done' | 'unreachable' | 'failed'> {
	const name = upstream.value?.name ?? '';
	promoting.value = true;
	message.value = info('promoting');
	try {
		const result = await serverApi.promote(force);
		if (force) message.value = info('promotedForced');
		else
			message.value = {
				key: result.followed ? 'promoted' : 'promotedNotFollowed',
				values: { name },
			};
	} catch (error) {
		if (!force && error instanceof ApiError && error.reason === 'unreachable') {
			message.value = null;
			return 'unreachable';
		}
		message.value = upstreamError(error);
		return 'failed';
	} finally {
		promoting.value = false;
	}
	// A code of the old main means nothing here any more.
	invitedCode.drop();
	await load();
	await rescueFocus(heading.value);
	return 'done';
}

async function unlink(): Promise<void> {
	const name = upstream.value?.name ?? '';
	if (!(await ask({ title: 'unlinkTitle', name, text: 'unlinkText', ok: 'unlinkOk' }))) return;
	try {
		const result = await serverApi.unlink();
		message.value = info(result.forgotten ? 'unlinked' : 'unlinkedNotTold');
	} catch (error) {
		message.value = upstreamError(error);
		return;
	}
	// A code of the old main means nothing here any more.
	invitedCode.drop();
	await load();
	await rescueFocus(heading.value);
}
</script>
