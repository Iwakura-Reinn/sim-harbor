import test from 'node:test'
import assert from 'node:assert/strict'
import { createBoardSettingsFlow } from './board-settings-flow.js'
test('板子设置没有自动查询，写入须确认且请求只一次，未知后只读确认', async () => {
  const calls = []
  const flow = createBoardSettingsFlow({
    request: async (path, method, body) => {
      calls.push(body)
      if (body.action === 'heartbeat_set') throw Error('lost')
      return { seconds: 120 }
    },
  })
  assert.equal(calls.length, 0)
  await assert.rejects(flow.set(120, 110, false))
  assert.equal(calls.length, 0)
  await assert.rejects(flow.set(120, 110, true))
  await flow.set(120, 110, true)
  assert.equal(calls.length, 1)
  await flow.query('heartbeat_query')
  assert.equal(calls[1].action, 'heartbeat_query')
  assert(!calls[1].confirmed)
})
test('留言下发必须确认，查询不携带写入字段', async () => {
  const calls = []
  const flow = createBoardSettingsFlow({
    request: async (path, method, body) => {
      calls.push(body)
      return {
        voicemail: {
          amr_url: '',
          slots: [
            { slot: 1, mode: 0, tts: '' },
            { slot: 2, mode: 0, tts: '' },
          ],
        },
        verified: true,
      }
    },
  })
  await assert.rejects(flow.applyVoicemail('enable', { amr_url: '', slots: [] }, false))
  assert.equal(calls.length, 0)
  await flow.query('voicemail_query')
  assert.equal(calls[0].action, 'voicemail_query')
  assert.equal(calls[0].confirmed, undefined)
  const expected = {
    amr_url: '',
    slots: [
      { slot: 1, mode: 0, tts: '' },
      { slot: 2, mode: 0, tts: '' },
    ],
  }
  const saved = await flow.applyVoicemail('restore', expected, true)
  assert.equal(saved.verified, true)
  assert.equal(calls[1].action, 'voicemail_apply')
  assert.equal(calls[1].intent, 'restore')
  assert.equal(calls[1].confirmed, true)
})
test('板子设置结果不完整不声称成功，不支持任意命令', async () => {
  const flow = createBoardSettingsFlow({ request: async () => ({ seconds: 120 }) })
  await assert.rejects(flow.query('restart'))
  await assert.rejects(flow.query('storage_query'))
  await assert.rejects(flow.set(120, 110, true))
  assert.equal(await flow.set(120, 110, true), null)
})
