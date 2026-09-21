// Presentation mapping for session and runtime state. Status is never
// communicated by color alone: every state carries an explicit text
// label plus a distinct icon/shape, with color purely supplementary.
import type { SessionInfo } from '$lib/api/types';
import {
	IconActivity,
	IconAlertTriangle,
	IconCircleDot,
	IconPlayerPause,
	IconProgress,
	IconQuestionMark,
	IconSnowflake,
	IconSquare,
	type Icon
} from '@tabler/icons-svelte';

export interface StatusPresentation {
	label: string;
	icon: Icon;
	/** Supplementary styling — the label and icon carry the meaning. */
	variant: 'default' | 'secondary' | 'outline' | 'destructive';
}

const known: Record<string, StatusPresentation> = {
	cold: { label: 'Cold', icon: IconSnowflake, variant: 'outline' },
	warm: { label: 'Warm', icon: IconCircleDot, variant: 'secondary' },
	starting: { label: 'Starting', icon: IconProgress, variant: 'secondary' },
	stopping: { label: 'Stopping', icon: IconPlayerPause, variant: 'secondary' },
	active: { label: 'Active', icon: IconActivity, variant: 'default' },
	idle: { label: 'Idle', icon: IconSquare, variant: 'outline' },
	waiting_input: { label: 'Waiting input', icon: IconAlertTriangle, variant: 'destructive' }
};

export function runtimeStatus(s: SessionInfo): StatusPresentation {
	return known[s.runtimeState] ?? { label: s.runtimeState, icon: IconQuestionMark, variant: 'outline' };
}

export function activityStatus(s: SessionInfo): StatusPresentation {
	return known[s.activity] ?? { label: s.activity, icon: IconQuestionMark, variant: 'outline' };
}

const providerNames: Record<string, string> = {
	codex: 'Codex',
	devin: 'Devin',
	opencode: 'OpenCode'
};

/** The wire field is `harness`; the UI presents it as Provider. */
export function providerName(harness: string): string {
	return providerNames[harness] ?? harness;
}
