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
test('固件拒绝 TCP 时，确认请求才携带短信号码和密码', async () => {
  const calls = []
  const smsState = {
    channel: 'sms',
    amr_url: '',
    slots: [
      { slot: 1, mode: null, tts: null },
      { slot: 2, mode: null, tts: null },
    ],
  }
  const flow = createBoardSettingsFlow({
    request: async (path, method, body, options) => {
      calls.push({ body, options })
      return { voicemail: smsState, verified: true }
    },
  })
  await flow.query('voicemail_query')
  assert.equal(calls[0].body.sms_phone, undefined)
  assert.equal(calls[0].body.sms_password, undefined)
  const restored = {
    channel: 'sms',
    amr_url: '',
    slots: [
      { slot: 1, mode: 0, tts: '' },
      { slot: 2, mode: 0, tts: '' },
    ],
  }
  await flow.applyVoicemail('enable', restored, true, { phone: '13800138000', password: '000000' })
  assert.equal(calls[1].body.action, 'voicemail_query')
  assert.equal(calls[1].body.sms_phone, undefined)
  assert.equal(calls[2].body.action, 'voicemail_apply')
  assert.equal(calls[2].body.intent, 'enable')
  assert.equal(calls[2].body.expected.slots[1].mode, null)
  assert.equal(calls[2].body.expected.slots[1].tts, null)
  assert.equal(calls[2].body.sms_phone, '13800138000')
  assert.equal(calls[2].body.sms_password, '000000')
  assert.equal(calls[2].options.timeout, 90000)
})
test('板子设置结果不完整不声称成功，不支持任意命令', async () => {
  const flow = createBoardSettingsFlow({ request: async () => ({ seconds: 120 }) })
  await assert.rejects(flow.query('restart'))
  await assert.rejects(flow.query('storage_query'))
  await assert.rejects(flow.set(120, 110, true))
  assert.equal(await flow.set(120, 110, true), null)
})
