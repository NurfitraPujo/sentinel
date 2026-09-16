import type { HandleClientError } from '@sveltejs/kit';

/**
 * Handle client-side errors, particularly version skew / chunk load failures.
 *
 * When a new deployment rolls out, Vite generates new hashed chunks. An active
 * browser tab holding an older HTML page will attempt to fetch old chunks that
 * no longer exist on the server (returning 404). This causes dynamic import
 * failures like "Failed to fetch dynamically imported module".
 *
 * Catching chunk 404s here and triggering a hard reload refreshes the client
 * to load the new deployment assets. A sessionStorage reload guard prevents
 * infinite refresh loops if an asset is genuinely missing.
 */
export const handleError: HandleClientError = async ({ error, event, status, message }) => {
	const err = error as { message?: string; name?: string; stack?: string } | undefined;
	const errorMessage = err?.message || String(error || '');

	const isDynamicImportError =
		errorMessage.includes('Failed to fetch dynamically imported module') ||
		errorMessage.includes('error loading dynamically imported module') ||
		errorMessage.includes('Importing a module script failed') ||
		errorMessage.includes('Loading chunk') ||
		errorMessage.includes('CSS chunk load failed');

	if (typeof window !== 'undefined' && isDynamicImportError) {
		const RELOAD_KEY = 'sentinel_version_skew_reload';
		const lastReload = sessionStorage.getItem(RELOAD_KEY);
		const now = Date.now();

		// Allow reload if never reloaded or last reload was more than 10 seconds ago
		if (!lastReload || now - parseInt(lastReload, 10) > 10000) {
			sessionStorage.setItem(RELOAD_KEY, String(now));
			window.location.reload();
			return {
				message: 'Updating to the latest version...'
			};
		}
	}

	return {
		message: message || 'An unexpected error occurred'
	};
};
