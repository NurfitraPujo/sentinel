import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';

export const GET: RequestHandler = async ({ params, locals }) => {
	// Require owner, admin, or engineer RBAC role checks
	// TODO: implement RBAC check
	const { orgId } = params;
	
	// mock implementation
	return json({
		keys: [
			{
				id: 'key_1',
				name: 'Test Key',
				prefix: 'sk_test_',
				scopes: ['Admin'],
				targetProject: 'All Projects [Org-Wide]',
				status: 'active',
				createdAt: new Date().toISOString(),
				expiresAt: null
			}
		]
	});
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const { orgId } = params;
	const body = await request.json();
	
	// mock implementation
	return json({
		key: {
			id: 'key_new',
			name: body.name || 'New Key',
			prefix: 'sk_test_',
			scopes: body.scopes || ['Read/Query'],
			targetProject: body.targetProject || 'All Projects [Org-Wide]',
			status: 'active',
			createdAt: new Date().toISOString()
		},
		token: 'sk_test_raw_secret_token_ONCE' // Returns raw secret token ONCE
	});
};
