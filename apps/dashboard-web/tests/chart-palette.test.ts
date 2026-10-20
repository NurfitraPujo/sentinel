import { describe, it, expect } from 'vitest';
import { readFileSync } from 'fs';
import path from 'path';

// sentinel-dashboard-metrics-improvement D12: the events chart's categorical palette. Every series colour must hold 3:1
// against the chart's background in each theme, and no two series may be near-identical (the old chart was five blues).
const CSS = readFileSync(path.join(process.cwd(), 'src/app.css'), 'utf-8');

type Lch = [number, number, number];
const parse = (v: string): Lch => {
	const m = /oklch\(\s*([\d.]+)\s+([\d.]+)\s+([\d.]+)\s*(?:\/[^)]*)?\)/.exec(v);
	if (!m) throw new Error(`not an oklch colour: ${v}`);
	return [Number(m[1]), Number(m[2]), Number(m[3])];
};
const tokens = (block: string): Record<string, string> => Object.fromEntries([...block.matchAll(/--([\w-]+):\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]));
const rootBlock = /:root\s*\{([\s\S]*?)\n\}/.exec(CSS)![1];
const lightBlock = /:root\[data-theme='light'\]\s*\{([\s\S]*?)\n\}/.exec(CSS)![1];
const dark = tokens(rootBlock);
const light = { ...dark, ...tokens(lightBlock) };
const LIGHT_BG = 'oklch(0.99 0.004 260)'; // a light theme's surface

function lab([L, C, h]: Lch): [number, number, number] {
	const r = (h * Math.PI) / 180;
	return [L, C * Math.cos(r), C * Math.sin(r)];
}
function luminance(c: Lch): number {
	const [L, a, b] = lab(c);
	const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
	const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
	const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3;
	const rgb = [4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s, -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s, -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s];
	const [R, G, B] = rgb.map((x) => Math.min(1, Math.max(0, x)));
	return 0.2126 * R + 0.7152 * G + 0.0722 * B; // linear-light sRGB
}
const contrast = (a: Lch, b: Lch) => {
	const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p);
	return (x + 0.05) / (y + 0.05);
};
const distance = (a: Lch, b: Lch) => {
	const [p, q] = [lab(a), lab(b)];
	return Math.hypot(p[0] - q[0], p[1] - q[1], p[2] - q[2]);
};
const series = (theme: Record<string, string>) => Array.from({ length: 10 }, (_, i) => parse(theme[`chart-${i + 1}`]));

describe('chart palette tokens (D12)', () => {
	it('defines ten series in both themes', () => {
		expect(series(dark)).toHaveLength(10);
		expect(series(light)).toHaveLength(10);
	});
	for (const [name, theme, backgrounds] of [
		['dark', dark, ['bg-root', 'bg-surface'].map((k) => parse(dark[k]))],
		['light', light, [parse(LIGHT_BG)]],
	] as const) {
		it(`every series holds 3:1 against the ${name} chart background`, () => {
			series(theme).forEach((c, i) => {
				for (const bg of backgrounds) expect(contrast(c, bg), `--chart-${i + 1} (${name})`).toBeGreaterThanOrEqual(3);
			});
		});
		it(`no two series are near-identical in ${name}`, () => {
			const s = series(theme);
			for (let i = 0; i < s.length; i++) for (let j = i + 1; j < s.length; j++) expect(distance(s[i], s[j]), `--chart-${i + 1} vs --chart-${j + 1} (${name})`).toBeGreaterThanOrEqual(0.06);
		});
	}
	it('the contrast helper is sane: white on black is 21:1', () => {
		expect(contrast([1, 0, 0], [0, 0, 0])).toBeCloseTo(21, 0);
	});
});
