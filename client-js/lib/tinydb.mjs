// ESM-обёртка над CommonJS-ядром: import { Client } from 'tinydb-client';
import mod from './tinydb.js';

export const Client = mod.Client;
export const APIError = mod.APIError;
export const isStatus = mod.isStatus;
export const DEFAULT_TIMEOUT_MS = mod.DEFAULT_TIMEOUT_MS;
export default mod;
