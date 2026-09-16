// `sentinel-sourcemaps` — the package's `bin` (contract §4.1, §4.8). scripts/build.mjs prepends the shebang.
//
// Scaffold (sentinel-sdk-js/TASK-01): the file exists so the `bin` link resolves on install; the CLI itself
// lands in task 8. Until then every invocation says so on stderr and exits 2.

process.stderr.write('sentinel-sourcemaps: not implemented yet (sentinel-sdk-js task 8, contract §4.8)\n');
process.exit(2);
