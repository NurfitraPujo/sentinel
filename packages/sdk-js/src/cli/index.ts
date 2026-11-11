// `sentinel-sourcemaps` — the package's `bin` (contract §4.1, §4.8). scripts/build.mjs prepends the shebang.
// The program is `runCli` (main.ts); this binds it to the process: `process.exitCode`, not `process.exit`, so a
// piped stdout is flushed before the exit.

import { runCli } from './main.js';

process.exitCode = await runCli(process.argv.slice(2), {
  stdout: (text) => {
    process.stdout.write(text);
  },
  stderr: (text) => {
    process.stderr.write(text);
  },
  env: process.env,
});
