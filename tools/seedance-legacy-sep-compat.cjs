// Uses SEP's real adapter and schemas with simulated HTTP; never calls a provider.
const assert = require('node:assert/strict')
const { createHash } = require('node:crypto')
const { createRequire, Module } = require('node:module')
const { join, resolve } = require('node:path')
const { execFileSync } = require('node:child_process')
const { mkdtemp, readFile, rm } = require('node:fs/promises')
const { tmpdir } = require('node:os')

const [sepRoot, dependencies] = process.argv.slice(2)
if (!sepRoot || !dependencies) {
  throw new Error('Usage: node tools/seedance-legacy-sep-compat.cjs <SEP root> <SEP backend node_modules>')
}
const backend = join(resolve(sepRoot), 'backend')
process.env.NODE_PATH = resolve(dependencies)
Module._initPaths()
const external = createRequire(join(resolve(dependencies), 'compat.cjs'))
external('ts-node').register({
  project: join(backend, 'tsconfig.json'),
  transpileOnly: true,
  compilerOptions: { incremental: false },
})
external('tsconfig-paths').register({ baseUrl: join(backend, 'src'), paths: { '*': ['*'] } })
const { Sub2apiVideoAdapter } = require(join(backend, 'src/modules/media-gateway/sub2api-video.adapter.ts'))
const { VideoFileCacheService } = require(join(backend, 'src/modules/media-gateway/video-file-cache.service.ts'))

async function main() {
  const adapter = new Sub2apiVideoAdapter({})
  const base = 'https://relay.example/v1'
  const key = 'test-only-key'
  const connection = { base, key, fingerprint: createHash('sha256').update(base + '\0' + key).digest('hex') }
  const taskId = 'mtask_00000000000000000000000000'
  const nativeFetch = global.fetch
  const temporary = await mkdtemp(join(tmpdir(), 'longdao-sep-video-'))
  const previousCache = process.env.MEDIA_VIDEO_CACHE_DIR
  process.env.MEDIA_VIDEO_CACHE_DIR = join(temporary, 'cache')
  let file
  let responseFactory
  global.fetch = async (url, options) => {
    assert.equal(new Headers(options.headers).get('Authorization'), 'Bearer ' + key)
    assert.equal(options.redirect, 'error')
    assert.ok(url.startsWith(base + '/videos/'))
    return responseFactory()
  }
  try {
    execFileSync('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-f', 'lavfi', '-i',
      'testsrc=size=128x72:rate=10', '-t', '1', '-c:v', 'libx264', '-pix_fmt', 'yuv420p',
      '-movflags', '+faststart', join(temporary, 'source.mp4')], { windowsHide: true })
    file = await readFile(join(temporary, 'source.mp4'))
    for (const status of ['queued', 'in_progress', 'completed', 'failed', 'cancelled']) {
      responseFactory = () => new Response(JSON.stringify({ object: 'video', id: taskId, model: 'seedance-2.0', status }),
        { headers: { 'Content-Type': 'application/json' } })
      const task = await adapter.query(connection, taskId, 'seedance-2.0')
      assert.equal(task.status, status)
    }
    for (const withLength of [true, false]) {
      responseFactory = () => new Response(file, { headers: {
        'Content-Type': 'video/mp4', ...(withLength ? { 'Content-Length': String(file.length) } : {}),
      } })
      const response = await adapter.rawContent(connection, taskId)
      assert.equal(response.status, 200)
      assert.deepEqual(Buffer.from(await response.arrayBuffer()), file)
    }
    responseFactory = () => new Response(file, { headers: { 'Content-Type': 'video/mp4', 'Content-Length': String(file.length + 1) } })
    const truncated = await adapter.rawContent(connection, taskId)
    await assert.rejects(() => truncated.arrayBuffer())
    responseFactory = () => new Response('bad file', { headers: { 'Content-Type': 'text/html' } })
    await assert.rejects(() => adapter.rawContent(connection, taskId))
    responseFactory = () => new Response(file, { headers: { 'Content-Type': 'video/mp4' } })
    const cache = new VideoFileCacheService()
    await cache.assertAvailable()
    const expiresAt = new Date(Date.now() + 60_000).toISOString()
    const metadata = await cache.saveOutput('owner-user', 'output-one', await adapter.rawContent(connection, taskId), expiresAt)
    assert.equal(metadata.sha256, createHash('sha256').update(file).digest('hex'))
    assert.equal(metadata.byteLength, file.length)
    assert.equal(metadata.width, 128)
    assert.equal(metadata.height, 72)
    assert.equal(metadata.durationSeconds, 1)
    assert.equal(metadata.frameRate, 10)
    assert.equal(metadata.hasAudio, false)
    const expected = { sha256: metadata.sha256, byteLength: metadata.byteLength, expiresAt }
    const output = await cache.streamOutput('owner-user', 'output-one', expected)
    const chunks = []
    for await (const chunk of output.stream) chunks.push(chunk)
    assert.deepEqual(Buffer.concat(chunks), file)
    await assert.rejects(() => cache.streamOutput('different-user', 'output-one', expected))
    console.log('SEP real adapter/cache: task states, exact downloads, invalid files, ffprobe, SHA-256, persisted output and scoped reads passed.')
  } finally {
    global.fetch = nativeFetch
    if (previousCache === undefined) delete process.env.MEDIA_VIDEO_CACHE_DIR
    else process.env.MEDIA_VIDEO_CACHE_DIR = previousCache
    await rm(temporary, { recursive: true, force: true })
  }
}
main().catch(error => { console.error(error); process.exitCode = 1 })
