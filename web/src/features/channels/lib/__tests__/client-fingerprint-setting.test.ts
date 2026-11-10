/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { describe, expect, test } from 'vitest'

import {
  buildSettingJSON,
  CHANNEL_FORM_DEFAULT_VALUES,
  isClientFingerprintMode,
  transformChannelToFormDefaults,
} from '../channel-form'

const baseForm = {
  ...CHANNEL_FORM_DEFAULT_VALUES,
  type: 1,
} as Parameters<typeof buildSettingJSON>[0]

const minChannel = (setting: string) =>
  ({
    id: 1,
    type: 1,
    setting,
    channel_info: {},
  }) as never

describe('client_fingerprint setting round-trip', () => {
  test('auto (default) does not write the key, keeping unchanged JSON equivalent', () => {
    const json = JSON.parse(buildSettingJSON(baseForm))
    expect('client_fingerprint' in json).toBe(false)
  })

  test('codex / claude / off are written verbatim', () => {
    for (const mode of ['codex', 'claude', 'off'] as const) {
      const json = JSON.parse(
        buildSettingJSON({ ...baseForm, client_fingerprint: mode })
      )
      expect(json.client_fingerprint).toBe(mode)
    }
  })

  test('legacy setting JSON without the key parses back to auto', () => {
    const form = transformChannelToFormDefaults(
      minChannel(JSON.stringify({ pass_through_body_enabled: true }))
    )
    expect(form.client_fingerprint).toBe('auto')
  })

  test('existing codex value parses back verbatim; unknown value falls back to auto', () => {
    const codex = transformChannelToFormDefaults(
      minChannel(JSON.stringify({ client_fingerprint: 'codex' }))
    )
    expect(codex.client_fingerprint).toBe('codex')

    const legacy = transformChannelToFormDefaults(
      minChannel(JSON.stringify({ client_fingerprint: 'sdk' }))
    )
    expect(legacy.client_fingerprint).toBe('auto')
  })

  test('isClientFingerprintMode guards parsed JSON values', () => {
    expect(isClientFingerprintMode('codex')).toBe(true)
    expect(isClientFingerprintMode('auto')).toBe(true)
    expect(isClientFingerprintMode('CODEX')).toBe(false)
    expect(isClientFingerprintMode(1)).toBe(false)
    expect(isClientFingerprintMode(undefined)).toBe(false)
  })
})
