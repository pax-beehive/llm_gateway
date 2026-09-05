import { build } from 'esbuild';
const result = await build({ entryPoints: ['tests/models.test.tsx'], bundle: true, write: false, platform: 'node', format: 'esm', jsx: 'automatic' });
await import(`data:text/javascript;base64,${Buffer.from(result.outputFiles[0].text).toString('base64')}`);
