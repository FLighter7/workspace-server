import type { ThemeDefinition } from 'vuetify';

/**
 * The page's own palette, the one its plain CSS had: a blue accent, red for
 * what deletes, green for what is up. Every pair is text on its background.
 */
export const THEMES: Record<'light' | 'dark', ThemeDefinition> = {
	light: {
		dark: false,
		colors: {
			background: '#F5F6F8',
			surface: '#FFFFFF',
			'surface-variant': '#E9ECF1',
			'on-surface-variant': '#586174',
			'on-background': '#1C2230',
			'on-surface': '#1C2230',
			outline: '#D5DAE3',
			primary: '#2B5BCC',
			'on-primary': '#FFFFFF',
			error: '#B0261D',
			'on-error': '#FFFFFF',
			success: '#1B7539',
			'on-success': '#FFFFFF',
			warning: '#634700',
			'on-warning': '#FFFFFF',
			info: '#2B5BCC',
			'on-info': '#FFFFFF',
		},
		variables: {
			'high-emphasis-opacity': 0.87,
			'medium-emphasis-opacity': 0.72,
		},
	},
	dark: {
		dark: true,
		colors: {
			background: '#13161B',
			surface: '#1C2028',
			'surface-variant': '#2A303B',
			'on-surface-variant': '#9AA3B3',
			'on-background': '#E5E8ED',
			'on-surface': '#E5E8ED',
			outline: '#353B47',
			primary: '#8DB0FF',
			'on-primary': '#0D1320',
			error: '#FF8F85',
			'on-error': '#210B08',
			success: '#7FD89E',
			'on-success': '#13161B',
			warning: '#F2D27C',
			'on-warning': '#13161B',
			info: '#8DB0FF',
			'on-info': '#0D1320',
		},
		variables: {
			'high-emphasis-opacity': 1,
			'medium-emphasis-opacity': 0.72,
		},
	},
};
