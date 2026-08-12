import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import UploadZone from './UploadZone.svelte';

// Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4/§10): the upload zone POSTs each
// picked/dropped file to /api/uploads individually, tracks per-file status, and reports the set
// of successfully-uploaded attachment ids via `onchange` -- exactly what /reports/new sends as
// `attachmentIds[]` on report creation. Mocks `fetch`, matching this repo's other component test
// convention (see IssueRelations.test.ts).
describe('UploadZone', () => {
	beforeEach(() => {
		vi.restoreAllMocks();
	});

	function makeFile(name = 'photo.png', type = 'image/png', content = 'x'): File {
		return new File([content], name, { type });
	}

	function selectFiles(input: HTMLElement, files: File[]) {
		Object.defineProperty(input, 'files', { value: files, configurable: true });
		return fireEvent.change(input);
	}

	it('uploads a picked file to /api/uploads with the organizationId and reports the new attachment id via onchange', async () => {
		const fetchMock = vi.fn().mockResolvedValue({
			ok: true,
			json: async () => ({
				id: 'att-1',
				url: '/api/attachments/att-1',
				filename: 'photo.png',
				contentType: 'image/png',
				sizeBytes: 1,
			}),
		});
		vi.stubGlobal('fetch', fetchMock);

		const onchange = vi.fn();
		render(UploadZone, { organizationId: 'org-1', onchange });

		const input = screen.getByLabelText(/choose files/i);
		await selectFiles(input, [makeFile()]);
		await Promise.resolve();
		await Promise.resolve();

		expect(fetchMock).toHaveBeenCalledWith('/api/uploads', expect.objectContaining({ method: 'POST' }));
		const call = fetchMock.mock.calls[0];
		const formData = call[1].body as FormData;
		expect(formData.get('organizationId')).toBe('org-1');
		expect(formData.get('file')).toBeInstanceOf(File);

		expect(await screen.findByText(/uploaded/i)).toBeTruthy();
		expect(onchange).toHaveBeenLastCalledWith(['att-1']);
	});

	it('shows an error status and does not include the file in onchange when the upload fails', async () => {
		const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 415, json: async () => ({ message: 'bad type' }) });
		vi.stubGlobal('fetch', fetchMock);

		const onchange = vi.fn();
		render(UploadZone, { organizationId: 'org-1', onchange });

		const input = screen.getByLabelText(/choose files/i);
		await selectFiles(input, [makeFile('evil.exe', 'application/octet-stream')]);
		await Promise.resolve();
		await Promise.resolve();

		expect(await screen.findByText(/bad type/i)).toBeTruthy();
		expect(onchange).toHaveBeenLastCalledWith([]);
	});

	it('removing an uploaded file DELETEs /api/attachments/<id> and drops it from onchange', async () => {
		const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
			if (url === '/api/uploads') {
				return Promise.resolve({
					ok: true,
					json: async () => ({
						id: 'att-2',
						url: '/api/attachments/att-2',
						filename: 'photo.png',
						contentType: 'image/png',
						sizeBytes: 1,
					}),
				});
			}
			return Promise.resolve({ ok: true, json: async () => ({ success: true }) });
		});
		vi.stubGlobal('fetch', fetchMock);

		const onchange = vi.fn();
		render(UploadZone, { organizationId: 'org-1', onchange });

		const input = screen.getByLabelText(/choose files/i);
		await selectFiles(input, [makeFile()]);
		await Promise.resolve();
		await Promise.resolve();
		await screen.findByText(/uploaded/i);

		const removeBtn = screen.getByRole('button', { name: /remove photo\.png/i });
		await fireEvent.click(removeBtn);
		await Promise.resolve();
		await Promise.resolve();

		expect(fetchMock).toHaveBeenCalledWith('/api/attachments/att-2', expect.objectContaining({ method: 'DELETE' }));
		expect(onchange).toHaveBeenLastCalledWith([]);
	});

	it('clicking "Insert into body" on an uploaded image calls oninsert with markdown image syntax pointing at the attachment URL', async () => {
		const fetchMock = vi.fn().mockResolvedValue({
			ok: true,
			json: async () => ({
				id: 'att-3',
				url: '/api/attachments/att-3',
				filename: 'screenshot.png',
				contentType: 'image/png',
				sizeBytes: 1,
			}),
		});
		vi.stubGlobal('fetch', fetchMock);

		const oninsert = vi.fn();
		render(UploadZone, { organizationId: 'org-1', oninsert });

		const input = screen.getByLabelText(/choose files/i);
		await selectFiles(input, [makeFile('screenshot.png')]);
		await Promise.resolve();
		await Promise.resolve();
		await screen.findByText(/uploaded/i);

		const insertBtn = screen.getByRole('button', { name: /insert into body/i });
		await fireEvent.click(insertBtn);

		expect(oninsert).toHaveBeenCalledWith('![screenshot.png](/api/attachments/att-3)');
	});

	it('does not offer "Insert into body" for a non-image upload', async () => {
		const fetchMock = vi.fn().mockResolvedValue({
			ok: true,
			json: async () => ({
				id: 'att-4',
				url: '/api/attachments/att-4',
				filename: 'notes.txt',
				contentType: 'text/plain',
				sizeBytes: 1,
			}),
		});
		vi.stubGlobal('fetch', fetchMock);

		render(UploadZone, { organizationId: 'org-1' });

		const input = screen.getByLabelText(/choose files/i);
		await selectFiles(input, [makeFile('notes.txt', 'text/plain')]);
		await Promise.resolve();
		await Promise.resolve();
		await screen.findByText(/uploaded/i);

		expect(screen.queryByRole('button', { name: /insert into body/i })).toBeNull();
	});
});
