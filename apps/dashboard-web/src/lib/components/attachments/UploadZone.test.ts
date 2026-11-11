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

	it('routes a file over 25 MB through presign -> XHR PUT -> finalize instead of the proxy POST', async () => {
		const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
			if (url === '/api/uploads/presign') {
				const body = JSON.parse((init?.body as string) ?? '{}');
				expect(body).toMatchObject({
					organizationId: 'org-1',
					filename: 'big.mp4',
					contentType: 'video/mp4',
				});
				return Promise.resolve({
					ok: true,
					json: async () => ({
						attachmentId: 'att-big',
						uploadUrl: 'https://minio.example/bucket/org-1/att-big?sig=abc',
						expiresAt: new Date().toISOString(),
					}),
				});
			}
			if (url === '/api/uploads/att-big/finalize') {
				expect(init?.method).toBe('POST');
				return Promise.resolve({
					ok: true,
					json: async () => ({
						id: 'att-big',
						url: '/api/attachments/att-big',
						filename: 'big.mp4',
						contentType: 'video/mp4',
						sizeBytes: 30 * 1024 * 1024,
					}),
				});
			}
			throw new Error(`unexpected fetch call to ${url}`);
		});
		vi.stubGlobal('fetch', fetchMock);

		let capturedUrl: string | undefined;
		let capturedMethod: string | undefined;
		let capturedBody: unknown;
		class FakeXHR {
			upload = { onprogress: null as ((e: { lengthComputable: boolean; loaded: number; total: number }) => void) | null };
			onload: (() => void) | null = null;
			onerror: (() => void) | null = null;
			status = 200;
			open(method: string, url: string) {
				capturedMethod = method;
				capturedUrl = url;
			}
			send(body: unknown) {
				capturedBody = body;
				this.upload.onprogress?.({ lengthComputable: true, loaded: 50, total: 100 });
				this.onload?.();
			}
		}
		vi.stubGlobal('XMLHttpRequest', FakeXHR as unknown as typeof XMLHttpRequest);

		const onchange = vi.fn();
		render(UploadZone, { organizationId: 'org-1', onchange });

		const bigFile = makeFile('big.mp4', 'video/mp4', 'x'.repeat(1));
		Object.defineProperty(bigFile, 'size', { value: 30 * 1024 * 1024 });

		const input = screen.getByLabelText(/choose files/i);
		await selectFiles(input, [bigFile]);
		await Promise.resolve();
		await Promise.resolve();
		await Promise.resolve();

		expect(await screen.findByText(/uploaded/i)).toBeTruthy();
		expect(capturedMethod).toBe('PUT');
		expect(capturedUrl).toBe('https://minio.example/bucket/org-1/att-big?sig=abc');
		// The PUT body must be a type-stripped Blob (empty type => browser sends no Content-Type),
		// not the File itself: a signed-class Content-Type header the presigned URL did not sign is
		// a MinIO rejection. See UploadZone's xhrPut comment.
		expect(capturedBody).toBeInstanceOf(Blob);
		expect((capturedBody as Blob).type).toBe(''); // the load-bearing bit: no Content-Type gets sent
		expect(fetchMock).toHaveBeenCalledWith('/api/uploads/presign', expect.objectContaining({ method: 'POST' }));
		expect(fetchMock).toHaveBeenCalledWith('/api/uploads/att-big/finalize', expect.objectContaining({ method: 'POST' }));
		expect(fetchMock).not.toHaveBeenCalledWith('/api/uploads', expect.anything());
		expect(onchange).toHaveBeenLastCalledWith(['att-big']);
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
