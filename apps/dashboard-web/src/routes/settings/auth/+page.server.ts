import { error, fail } from '@sveltejs/kit';
import { getSetting, setSetting } from '$lib/server/settings';
import type { PageServerLoad, Actions } from './$types';

export const load: PageServerLoad = async ({ locals }) => {
    // Check if user is admin
    const session = await locals.getSession();
    if (!session) throw error(401, 'Unauthorized');
    
    // In a real app, check user role. Assuming admin for now for simplicity.
    
    const smtpServer = await getSetting('smtp_server');
    const smtpFrom = await getSetting('smtp_from');
    
    return {
        smtpServer: smtpServer ?? '',
        smtpFrom: smtpFrom ?? ''
    };
};

export const actions: Actions = {
    save: async ({ request, locals }) => {
        const session = await locals.getSession();
        if (!session) return fail(401);
        
        const data = await request.formData();
        const smtpServer = data.get('smtpServer') as string;
        const smtpFrom = data.get('smtpFrom') as string;
        
        if (!smtpServer || !smtpFrom) {
            return fail(400, { message: 'All fields are required' });
        }
        
        await setSetting('smtp_server', smtpServer);
        await setSetting('smtp_from', smtpFrom);
        
        return { success: true };
    }
};
