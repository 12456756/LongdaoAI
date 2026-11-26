const assert = require('node:assert/strict')
const crypto = require('node:crypto')

const origin = 'http://127.0.0.1:28080'
const hash = value => crypto.createHash('sha256').update(value).digest('hex')

async function main() {
  assert.equal(process.env.SEP_COMPUTE_LIVE_TEST, 'local-isolated-only')
  assert.ok(process.env.COMPUTE_TEST_ADMIN_PASSWORD, 'An isolated fixture admin password is required')
  assert.ok(process.env.COMPUTE_TEST_PASSWORD_HASH, 'A fixture bcrypt password hash is required')
  const stamp = Date.now().toString()
  const enterpriseId = `n09-pagination-${stamp}`
  const signing = crypto.generateKeyPairSync('ed25519')
  const encryption = crypto.generateKeyPairSync('rsa', { modulusLength: 3072 })
  const signingId = `n09-sign-${stamp}`
  let token, previous, registered = false, configured = false
  const groups = []

  async function admin(method, path, body) {
    const response = await fetch(`${origin}/api/v1${path}`, {
      method, headers: { 'Content-Type': 'application/json', 'User-Agent': 'N09PaginationRegression/1.0', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body), redirect: 'error', signal: AbortSignal.timeout(15000),
    })
    assert.equal(response.status, 200, `${method} ${path} failed with ${response.status}`)
    const value = await response.json()
    return Object.hasOwn(value, 'data') ? value.data : value
  }

  async function signed(method, path, body, idempotency = '') {
    const bytes = body === undefined ? '' : JSON.stringify(body)
    const headers = { 'Content-Type': 'application/json', 'User-Agent': 'N09PaginationRegression/1.0',
      'X-SEP-Version': '1', 'X-SEP-Key-ID': signingId, 'X-SEP-Timestamp': String(Math.floor(Date.now() / 1000)),
      'X-SEP-Nonce': crypto.randomUUID(), 'X-SEP-Enterprise-ID': enterpriseId,
      ...(idempotency ? { 'Idempotency-Key': idempotency } : {}) }
    const canonical = ['sep-v1', signingId, headers['X-SEP-Timestamp'], headers['X-SEP-Nonce'], method, path,
      hash(bytes), hash(''), enterpriseId, '', idempotency, headers['Content-Type'], '', ''].join('\n')
    headers['X-SEP-Signature'] = crypto.sign(null, Buffer.from(canonical), signing.privateKey).toString('base64url')
    const response = await fetch(origin + path, { method, headers, body: body === undefined ? undefined : bytes,
      redirect: 'error', signal: AbortSignal.timeout(15000) })
    assert.equal(response.status, 200, `${method} ${path} failed with ${response.status}`)
    return response.json()
  }

  async function credit(sequence) {
    const id = `n09-credit-${stamp}-${sequence}`
    const receipt = await signed('POST', '/api/v1/sep/transfers', {
      contract_version: 1, enterprise_id: enterpriseId, transfer_id: id, direction: 'credit_compute',
      currency: 'CNY', amount_cny: '1.000000', funding_ref: id,
    }, id)
    assert.equal(receipt.status, 'committed')
    assert.equal(receipt.sequence, String(sequence))
  }

  try {
    token = (await admin('POST', '/auth/login', { email: process.env.COMPUTE_TEST_ADMIN_EMAIL ?? 'pricing-admin@local.test',
      password: process.env.COMPUTE_TEST_ADMIN_PASSWORD })).access_token
    previous = await admin('GET', '/admin/sep/config')
    assert.equal(previous.enabled, false, 'The fixture must not replace an enabled integration')
    for (const kind of ['chat', 'image', 'video']) groups.push(await admin('POST', '/admin/groups', {
      name: `n09-${kind}-${stamp}`, platform: 'openai', subscription_type: 'standard', rate_multiplier: 1, allow_image_generation: true,
    }))
    const encryptionId = `n09-enc-${stamp}`
    await admin('PUT', '/admin/sep/config', { ...previous, enabled: true, signing_key_id: signingId,
      signing_public_key: signing.publicKey.export({ type: 'spki', format: 'pem' }), encryption_key_id: encryptionId,
      encryption_public_key: encryption.publicKey.export({ type: 'spki', format: 'pem' }),
      chat_group_id: groups[0].id, image_group_id: groups[1].id, video_group_id: groups[2].id })
    configured = true
    const registration = `n09-register-${stamp}`
    await signed('POST', '/api/v1/sep/enterprises', { enterprise_id: enterpriseId, registration_ref: registration,
      company_name: `N09 pagination ${stamp}`, admin_email: `n09-${stamp}@example.test`, encryption_key_id: encryptionId,
      password_hash: process.env.COMPUTE_TEST_PASSWORD_HASH }, registration)
    registered = true
    for (let i = 1; i <= 12; i++) await credit(i)
    for (const limit of [3, 5]) {
      const sequences = []
      let after = '0'
      let snapshot = limit === 3 ? '0' : '12'
      for (let page = 0; ; page++) {
        assert.ok(page < 12, 'Pagination must terminate')
        const ledger = await signed('GET', `/api/v1/sep/enterprises/${enterpriseId}/ledger?after_sequence=${after}&snapshot_sequence=${snapshot}&limit=${limit}`)
        assert.equal(ledger.snapshot_sequence, '12')
        assert.equal(ledger.total_paid_cny, '12.000000')
        assert.equal(ledger.available_paid_cny, '12.000000')
        assert.equal(ledger.frozen_paid_cny, '0.000000')
        assert.ok(ledger.items.length > 0 && ledger.items.length <= limit)
        for (const item of ledger.items) {
          assert.equal(item.sequence, String(sequences.length + 1))
          sequences.push(item.sequence)
        }
        assert.equal(ledger.after_sequence, sequences.at(-1))
        assert.equal(ledger.has_more, sequences.length < 12)
        if (limit === 3 && page === 0) await credit(13)
        after = ledger.after_sequence
        snapshot = ledger.snapshot_sequence
        if (!ledger.has_more) break
      }
      assert.deepEqual(sequences, Array.from({ length: 12 }, (_, i) => String(i + 1)))
      console.log(`PASS: signed N09 limit=${limit}, sequences 1..12, no duplicates/omissions, fixed balance and watermark during a new credit`)
    }
    const latest = await signed('GET', `/api/v1/sep/enterprises/${enterpriseId}/ledger?after_sequence=12&snapshot_sequence=0&limit=3`)
    assert.equal(latest.snapshot_sequence, '13')
    assert.equal(latest.total_paid_cny, '13.000000')
    assert.deepEqual(latest.items.map(item => item.sequence), ['13'])
    assert.equal(latest.has_more, false)
    console.log(`PASS: new snapshot includes sequence 13; isolated evidence enterprise ${enterpriseId}`)
  } finally {
    const failures = []
    if (registered) try { await admin('PUT', `/admin/sep/enterprises/${enterpriseId}`, { status: 'disabled', groups: {} }) } catch { failures.push('disable fixture enterprise') }
    if (configured) try { await admin('PUT', '/admin/sep/config', previous) } catch { failures.push('restore integration configuration') }
    for (const group of groups) try { await admin('PUT', `/admin/groups/${group.id}`, { status: 'inactive' }) } catch { failures.push('disable fixture group') }
    if (failures.length) throw new Error(`Fixture cleanup requires attention: ${failures.join(', ')}`)
  }
}

main().catch(error => { console.error(error.message); process.exitCode = 1 })
