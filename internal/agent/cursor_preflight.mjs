// Public acpx metadata only. No runtime store, prompt or permission callbacks.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
let output = {status: 'indeterminate'};
try {
  const [major, minor] = process.versions.node.split('.').map(Number);
  if (major < 22 || major === 22 && minor < 13) throw new Error();
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  const require = createRequire(input.Entry);
  const root = fs.realpathSync(input.Root);
  const load = async (subpath) => {
    const resolved = fs.realpathSync(require.resolve(`acpx/${subpath}`));
    if (!resolved.startsWith(root + path.sep)) throw new Error();
    return import(pathToFileURL(resolved).href);
  };
  const { inspectAgentModels } = await load('runtime');
  const { createAgentRegistry } = await load('agent-registry');
  if (typeof inspectAgentModels !== 'function' || typeof createAgentRegistry !== 'function') throw new Error();
  const resolveExecutable = (command) => {
    const candidates = command.includes('/') || command.includes('\\') ? [command] : (process.env.PATH || '').split(path.delimiter).map(dir => path.join(dir, command));
    for (const candidate of candidates) {
      try { fs.accessSync(candidate, fs.constants.X_OK); if (fs.statSync(candidate).isFile()) return fs.realpathSync(candidate); } catch {}
    }
    return undefined;
  };
  const registry = createAgentRegistry({overrides: input.Raw ? {cursor: input.Raw} : {}, resolveExecutable, resolvePackageRoot: () => undefined});
  const inspection = await registry.inspect('cursor');
  if (inspection?.launch?.kind !== 'installed' || !Array.isArray(inspection.launch.argv) || !inspection.launch.argv.length || inspection.launch.argv.some(arg => typeof arg !== 'string' || !arg)) throw new Error();
  // Inspect parses raw commands itself. An installed command runner is still
  // not a direct ACP target, and could install packages while being probed.
  const runners = new Set(['npx','npm','pnpm','yarn','bun','bunx','corepack','deno','npx-cli.js','npm-cli.js','sh','bash','zsh','fish','env','cmd','powershell','pwsh','node','nodejs','python','python3','perl','ruby']);
  const launcher = path.basename(inspection.launch.argv[0]).replace(/\.(exe|cmd|bat)$/i, '').toLowerCase();
  if (runners.has(launcher)) throw new Error();
  const models = await inspectAgentModels({agentCommand: inspection.launch.argv, cwd: process.cwd(), timeoutMs: 15000, agentProcessEnv: {...process.env}});
  const ids = models?.availableModelIds;
  if (!Array.isArray(ids) || !ids.length || ids.some(id => typeof id !== 'string' || !id.trim())) throw new Error();
  let catalogBytes = 0;
  for (const id of ids) { catalogBytes += Buffer.byteLength(id, 'utf8'); if (catalogBytes > 16384) throw new Error(); }
  if (new Set(ids).size !== ids.length) throw new Error();
  if (models.currentModelId !== undefined && (typeof models.currentModelId !== 'string' || !ids.includes(models.currentModelId))) throw new Error();
  if (input.Model) output.status = ids.includes(input.Model) ? 'supported' : 'unsupported';
  else if (models.currentModelId) output.status = 'supported';
} catch {}
process.stdout.write(JSON.stringify(output));
