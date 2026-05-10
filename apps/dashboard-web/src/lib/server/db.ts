import { drizzle } from 'drizzle-orm/postgres-js';
import postgres from 'postgres';
import { env } from '$env/dynamic/private';

const connectionString = env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';

const client = postgres(connectionString);
export const db = drizzle(client);