import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import AttachmentList from './AttachmentList.svelte';

// Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4/§10): the report detail page's
// attachments section -- images render as inline thumbnails via the authorized
// /api/attachments/<id> URL, everything else renders as a download link showing filename + size.
describe('AttachmentList', () => {
	it('renders an image attachment as a thumbnail linking to the authorized attachment URL', () => {
		render(AttachmentList, {
			attachments: [
				{ id: 'att-1', filename: 'screenshot.png', contentType: 'image/png', sizeBytes: 2048 },
			],
		});

		const img = screen.getByAltText('screenshot.png') as HTMLImageElement;
		expect(img.getAttribute('src')).toBe('/api/attachments/att-1');

		const link = img.closest('a');
		expect(link?.getAttribute('href')).toBe('/api/attachments/att-1');
	});

	it('renders a non-image attachment as a download link with filename and human-readable size', () => {
		render(AttachmentList, {
			attachments: [
				{ id: 'att-2', filename: 'stacktrace.log', contentType: 'text/plain', sizeBytes: 1536 },
			],
		});

		const link = screen.getByRole('link', { name: /stacktrace\.log/i });
		expect(link.getAttribute('href')).toBe('/api/attachments/att-2');
		expect(link.getAttribute('download')).toBe('stacktrace.log');
		expect(screen.getByText('1.5 KB')).toBeTruthy();

		// No <img> for a non-image attachment.
		expect(screen.queryByRole('img')).toBeNull();
	});

	it('renders a fallback message when there are no attachments', () => {
		render(AttachmentList, { attachments: [] });

		expect(screen.getByText(/no attachments/i)).toBeTruthy();
		expect(screen.queryByTestId('attachment-list')).toBeNull();
	});
});

// sentinel-user-report-issue fe.md task 5 (row S13): the reporter's screenshot is often the most
// useful thing on a report, and when its bytes cannot be fetched the strip must say which file it
// was rather than show the browser's broken-image glyph. Asserted by firing the image's own `error`
// event, which is exactly what the browser does on a 404 or an expired object.
describe('AttachmentList image fallback (S13)', () => {
	it('falls back to the filename download link when the thumbnail fails to load', async () => {
		render(AttachmentList, {
			attachments: [
				{ id: 'att-3', filename: 'screen.png', contentType: 'image/png', sizeBytes: 184213 },
			],
		});

		const img = screen.getByAltText('screen.png') as HTMLImageElement;
		await fireEvent.error(img);

		expect(screen.queryByAltText('screen.png')).toBeNull();
		const link = await screen.findByRole('link', { name: /screen\.png/i });
		expect(link.getAttribute('href')).toBe('/api/attachments/att-3');
		expect(link.getAttribute('download')).toBe('screen.png');
		expect(screen.getByText('179.9 KB')).toBeTruthy();
	});

	it('leaves other thumbnails alone when one fails', async () => {
		render(AttachmentList, {
			attachments: [
				{ id: 'att-a', filename: 'a.png', contentType: 'image/png', sizeBytes: 10 },
				{ id: 'att-b', filename: 'b.png', contentType: 'image/png', sizeBytes: 10 },
			],
		});

		await fireEvent.error(screen.getByAltText('a.png'));

		expect(screen.queryByAltText('a.png')).toBeNull();
		expect(screen.getByAltText('b.png')).toBeTruthy();
	});
});
