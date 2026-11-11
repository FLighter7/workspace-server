<template>
	<div>
		<dt>
			<slot name="label">{{ label }}</slot>
		</dt>
		<dd><slot /></dd>
	</div>
</template>

<script setup lang="ts">
defineProps<{ label?: string }>();
</script>
