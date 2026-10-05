// Gets media-kit (the shared visualizer / upload / search library) ready for building and testing
// on this machine. Docker builds do not need this: the Dockerfile downloads media-kit itself.
//
// With a media-kit checkout (default ../media-kit):
//   go.work                  written if missing, so Go uses the checkout instead of the download
//   web/lib/media-kit/       its browser modules, copied from the checkout
// Without one:
//   web/lib/media-kit/       copied from the version go.mod names, downloaded by `go mod download`
//
// Neither file is committed. Run it again after changing media-kit:
//
//   node scripts/sync-media-kit.mjs [path to media-kit]

import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const MODULE = 'github.com/czyrustuazon/lib-szmy-media-kit';
const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const local = path.resolve(process.argv[2] || path.join(repo, '..', 'media-kit'));

// Copies every .js file under from to to, replacing whatever was at to.
function copyJS(from, to) {
  fs.rmSync(to, { recursive: true, force: true });
  let n = 0;
  const walk = (rel) => {
    for (const e of fs.readdirSync(path.join(from, rel), { withFileTypes: true })) {
      const r = path.join(rel, e.name);
      if (e.isDirectory()) walk(r);
      else if (r.endsWith('.js')) {
        fs.mkdirSync(path.dirname(path.join(to, r)), { recursive: true });
        fs.copyFileSync(path.join(from, r), path.join(to, r));
        fs.chmodSync(path.join(to, r), 0o644); // the Go module cache is read-only
        n++;
      }
    }
  };
  walk('');
  return n;
}

let src;
let source;
if (fs.existsSync(path.join(local, 'go.mod')) && fs.existsSync(path.join(local, 'js'))) {
  src = local;
  source = `checkout ${local}`;
  const work = path.join(repo, 'go.work');
  if (!fs.existsSync(work)) {
    const rel = path.relative(repo, local).split(path.sep).join('/');
    fs.writeFileSync(work, `go 1.27\n\nuse (\n\t.\n\t${rel}\n)\n`);
    console.log(`wrote go.work: Go uses ${rel} instead of the downloaded media-kit`);
  }
} else {
  const r = spawnSync('go', ['mod', 'download', '-json', MODULE], { cwd: repo, encoding: 'utf8', env: { ...process.env, GOWORK: 'off' } });
  if (r.error || r.status !== 0) {
    console.error(`no media-kit checkout at ${local}, and downloading it needs Go:`);
    console.error((r.error && r.error.message) || r.stdout || r.stderr);
    process.exit(1);
  }
  const mod = JSON.parse(r.stdout);
  src = mod.Dir;
  source = `${MODULE} ${mod.Version} (downloaded)`;
}

const n = copyJS(path.join(src, 'js'), path.join(repo, 'web', 'lib', 'media-kit'));
console.log(`web/lib/media-kit: ${n} JS files from ${source}`);
