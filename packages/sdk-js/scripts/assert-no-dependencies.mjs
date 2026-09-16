// The no-runtime-dependencies assertion (contract §4.1, §11): `dependencies` is ABSENT from package.json —
// not empty, absent. Run by the sdk-js-node CI job on both node versions and by `pnpm check:no-dependencies`.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const path = join(process.cwd(), 'package.json');
const pkg = JSON.parse(readFileSync(path, 'utf8'));

if (Object.prototype.hasOwnProperty.call(pkg, 'dependencies')) {
  console.error(`${path}: a "dependencies" key is present — @sentinel/sdk-js ships no runtime dependencies (contract §4.1)`);
  process.exit(1);
}
console.log(`${pkg.name}: no "dependencies" key — ok`);
