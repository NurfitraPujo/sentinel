import { db } from './db';
import { settings } from '../db/schema';
import { eq } from 'drizzle-orm';
import crypto from 'crypto';
import { env } from '$env/dynamic/private';

const ENCRYPTION_KEY = env.SETTINGS_ENCRYPTION_KEY || 'default-long-and-secure-key-32-chars'; // 32 chars

export async function getSetting(key: string): Promise<string | null> {
    const result = await db.select().from(settings).where(eq(settings.key, key)).limit(1);
    if (result.length === 0) return null;
    return decrypt(result[0].value);
}

export async function setSetting(key: string, value: string): Promise<void> {
    const encryptedValue = encrypt(value);
    const existing = await db.select().from(settings).where(eq(settings.key, key)).limit(1);
    
    if (existing.length > 0) {
        await db.update(settings).set({ value: encryptedValue, updatedAt: new Date() }).where(eq(settings.key, key));
    } else {
        await db.insert(settings).values({ key, value: encryptedValue });
    }
}

function encrypt(text: string): string {
    const iv = crypto.randomBytes(16);
    const cipher = crypto.createCipheriv('aes-256-cbc', Buffer.from(ENCRYPTION_KEY), iv);
    let encrypted = cipher.update(text);
    encrypted = Buffer.concat([encrypted, cipher.final()]);
    return iv.toString('hex') + ':' + encrypted.toString('hex');
}

function decrypt(text: string): string {
    const textParts = text.split(':');
    const iv = Buffer.from(textParts.shift()!, 'hex');
    const encryptedText = Buffer.from(textParts.join(':'), 'hex');
    const decipher = crypto.createDecipheriv('aes-256-cbc', Buffer.from(ENCRYPTION_KEY), iv);
    let decrypted = decipher.update(encryptedText);
    decrypted = Buffer.concat([decrypted, decipher.final()]);
    return decrypted.toString();
}
