import { createClient, type Environment } from '@sshai/contracts';

const client = createClient({ baseUrl: 'http://localhost:8080/v1' });
type EnvironmentContract = Environment;

void client;
export type { EnvironmentContract };
