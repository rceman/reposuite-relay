// Small display-format helpers shared by the dashboard views.

/** "3d 4h", "5h 12m", "42m", "30s" — compact daemon uptime. */
export function formatUptime(seconds: number): string {
	const s = Math.max(0, Math.floor(seconds));
	const d = Math.floor(s / 86400);
	const h = Math.floor((s % 86400) / 3600);
	const m = Math.floor((s % 3600) / 60);
	if (d > 0) return `${d}d ${h}h`;
	if (h > 0) return `${h}h ${m}m`;
	if (m > 0) return `${m}m ${s % 60}s`;
	return `${s}s`;
}

/** ISO timestamp → local "HH:MM:SS" for compact table cells. */
export function formatTime(iso: string): string {
	const t = new Date(iso);
	if (Number.isNaN(t.getTime())) return '—';
	return t.toLocaleTimeString(undefined, { hour12: false });
}
