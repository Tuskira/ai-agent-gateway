import { describe, expect, it } from 'vitest'
import {
  MAX_COMMAND_ARGUMENTS,
  MAX_SKILL_FILES,
  SKILL_FILE_MAX_BYTES,
  SKILL_MD_MAX_BYTES,
  commandArgumentError,
  findUndeclaredPlaceholders,
  parseFrontmatter,
  skillMdTemplate,
  validateCommandArgumentCount,
  validateCommandArgumentName,
  validateFrontmatter,
  validateSkillDescription,
  validateSkillFilePath,
  validateSkillFiles,
  validateSkillName,
} from '@/lib/skill-validation'

describe('validateSkillName', () => {
  it('accepts a valid name', () => {
    expect(validateSkillName('incident-runbook')).toBeNull()
    expect(validateSkillName('a')).toBeNull()
    expect(validateSkillName('a.b_c-9')).toBeNull()
  })

  it('rejects an empty name', () => {
    expect(validateSkillName('')).toMatch(/required/i)
    expect(validateSkillName('   ')).toMatch(/required/i)
  })

  it('rejects uppercase and other invalid characters', () => {
    expect(validateSkillName('Incident')).toMatch(/lowercase/i)
    expect(validateSkillName('incident runbook')).toMatch(/lowercase/i)
    expect(validateSkillName('_incident')).toMatch(/lowercase/i)
  })

  it('rejects a name over 64 characters', () => {
    expect(validateSkillName('a'.repeat(65))).toMatch(/lowercase/i)
    expect(validateSkillName('a'.repeat(64))).toBeNull()
  })

  it('rejects "__" (reserved for connector tool names)', () => {
    expect(validateSkillName('my__skill')).toMatch(/__/)
  })

  it('rejects the reserved name "gateway"', () => {
    expect(validateSkillName('gateway')).toMatch(/reserved/i)
  })
})

describe('validateSkillDescription', () => {
  it('allows an empty/undefined description', () => {
    expect(validateSkillDescription(undefined)).toBeNull()
    expect(validateSkillDescription('')).toBeNull()
  })

  it('rejects a description over 1024 characters', () => {
    expect(validateSkillDescription('a'.repeat(1025))).toMatch(/1024/)
    expect(validateSkillDescription('a'.repeat(1024))).toBeNull()
  })
})

describe('validateSkillFilePath', () => {
  it('accepts a valid nested path', () => {
    expect(validateSkillFilePath('SKILL.md')).toBeNull()
    expect(validateSkillFilePath('reference/notes.txt')).toBeNull()
  })

  it('rejects a leading or trailing slash', () => {
    expect(validateSkillFilePath('/SKILL.md')).toMatch(/start with/i)
    expect(validateSkillFilePath('dir/')).toMatch(/end with/i)
  })

  it('rejects ".." anywhere in the path', () => {
    expect(validateSkillFilePath('../SKILL.md')).toMatch(/\.\./)
    expect(validateSkillFilePath('a/../b.md')).toMatch(/\.\./)
  })

  it('rejects a disallowed extension', () => {
    expect(validateSkillFilePath('script.sh')).toMatch(/extension/i)
    expect(validateSkillFilePath('noext')).toMatch(/extension/i)
  })

  it('accepts every allowed extension', () => {
    for (const ext of ['.md', '.txt', '.json', '.yaml', '.yml', '.csv', '.xml', '.toml']) {
      expect(validateSkillFilePath(`file${ext}`)).toBeNull()
    }
  })
})

describe('validateSkillFiles', () => {
  it('requires at least one file and a root SKILL.md', () => {
    const result = validateSkillFiles([])
    expect(result.errors).toEqual(
      expect.arrayContaining([
        expect.stringMatching(/at least one file/i),
        expect.stringMatching(/SKILL\.md/),
      ]),
    )
  })

  it('accepts a minimal valid file set', () => {
    const result = validateSkillFiles([{ path: 'SKILL.md', content: 'hello' }])
    expect(result.errors).toEqual([])
    expect(result.fileErrors).toEqual([null])
  })

  it(`rejects more than ${MAX_SKILL_FILES} files`, () => {
    const files = Array.from({ length: MAX_SKILL_FILES + 1 }, (_, i) => ({
      path: i === 0 ? 'SKILL.md' : `f${i}.md`,
      content: 'x',
    }))
    const result = validateSkillFiles(files)
    expect(result.errors).toEqual(
      expect.arrayContaining([expect.stringMatching(/at most 20 files/i)]),
    )
  })

  it('flags duplicate paths', () => {
    const result = validateSkillFiles([
      { path: 'SKILL.md', content: 'a' },
      { path: 'notes.md', content: 'b' },
      { path: 'notes.md', content: 'c' },
    ])
    expect(result.fileErrors[1]).toMatch(/duplicate/i)
    expect(result.fileErrors[2]).toMatch(/duplicate/i)
  })

  it('flags a SKILL.md over 64 KiB', () => {
    const result = validateSkillFiles([
      { path: 'SKILL.md', content: 'a'.repeat(SKILL_MD_MAX_BYTES + 1) },
    ])
    expect(result.fileErrors[0]).toMatch(/64 KiB/)
  })

  it('flags a non-SKILL.md file over 256 KiB', () => {
    const result = validateSkillFiles([
      { path: 'SKILL.md', content: 'ok' },
      { path: 'big.txt', content: 'a'.repeat(SKILL_FILE_MAX_BYTES + 1) },
    ])
    expect(result.fileErrors[1]).toMatch(/256 KiB/)
  })

  it('flags a total size over 512 KiB even when every file is individually fine', () => {
    const result = validateSkillFiles([
      { path: 'SKILL.md', content: 'ok' },
      { path: 'a.txt', content: 'a'.repeat(SKILL_FILE_MAX_BYTES) },
      { path: 'b.txt', content: 'b'.repeat(SKILL_FILE_MAX_BYTES) },
      { path: 'c.txt', content: 'c'.repeat(SKILL_FILE_MAX_BYTES) },
    ])
    expect(result.errors).toEqual(
      expect.arrayContaining([expect.stringMatching(/total file size.*512 KiB/i)]),
    )
  })
})

describe('parseFrontmatter', () => {
  it('returns null when there is no frontmatter block', () => {
    expect(parseFrontmatter('just some text')).toBeNull()
  })

  it('splits a valid block into keys and body', () => {
    const parsed = parseFrontmatter('---\nname: foo\ndescription: does a thing\n---\nBody text\n')
    expect(parsed).not.toBeNull()
    expect(parsed!.keys.name).toBe('foo')
    expect(parsed!.keys.description).toBe('does a thing')
    expect(parsed!.body).toBe('Body text\n')
  })

  it('ignores indented (nested) lines', () => {
    const parsed = parseFrontmatter('---\nname: foo\nmetadata:\n  kind: command\n---\nBody\n')
    expect(parsed!.keys).toEqual({ name: 'foo', metadata: '' })
  })
})

describe('validateFrontmatter', () => {
  it('accepts the generated template for the matching name', () => {
    expect(validateFrontmatter(skillMdTemplate('incident-runbook', 'skill'), 'incident-runbook')).toEqual(
      expect.arrayContaining([]),
    )
    expect(validateFrontmatter(skillMdTemplate('incident-runbook', 'skill'), 'incident-runbook').length).toBe(
      0,
    )
  })

  it('requires frontmatter to be present at all', () => {
    expect(validateFrontmatter('no frontmatter here', 'foo')).toEqual([
      expect.stringMatching(/must start with yaml frontmatter/i),
    ])
  })

  it('requires "name" to equal the skill name', () => {
    const errors = validateFrontmatter('---\nname: other\ndescription: d\n---\nbody', 'foo')
    expect(errors).toEqual(
      expect.arrayContaining([expect.stringMatching(/"name" must equal the skill name/)]),
    )
  })

  it('requires a non-empty description', () => {
    const errors = validateFrontmatter('---\nname: foo\n---\nbody', 'foo')
    expect(errors).toEqual(
      expect.arrayContaining([expect.stringMatching(/non-empty "description"/)]),
    )
  })

  it('rejects the "hooks" key', () => {
    const errors = validateFrontmatter(
      '---\nname: foo\ndescription: d\nhooks: something\n---\nbody',
      'foo',
    )
    expect(errors).toEqual(expect.arrayContaining([expect.stringMatching(/"hooks".*not allowed/)]))
  })

  it('rejects an unknown frontmatter key', () => {
    const errors = validateFrontmatter(
      '---\nname: foo\ndescription: d\ntotally_unknown: x\n---\nbody',
      'foo',
    )
    expect(errors).toEqual(
      expect.arrayContaining([expect.stringMatching(/unknown frontmatter key "totally_unknown"/i)]),
    )
  })

  it('accepts every allowed Claude Code / Agent Skills key', () => {
    const fm = [
      '---',
      'name: foo',
      'description: d',
      'license: MIT',
      'compatibility: x',
      'allowed-tools: x',
      'user-invocable: true',
      'disable-model-invocation: false',
      'context: x',
      'agent: x',
      'background: false',
      'model: x',
      'effort: high',
      'paths: x',
      'shell: false',
      'arguments: x',
      'argument-hint: x',
      'when_to_use: x',
      '---',
      'body',
    ].join('\n')
    expect(validateFrontmatter(fm, 'foo')).toEqual([])
  })
})

describe('command arguments', () => {
  it('validates an argument name', () => {
    expect(validateCommandArgumentName('severity')).toBeNull()
    expect(validateCommandArgumentName('Severity')).toMatch(/lowercase/i)
    expect(validateCommandArgumentName('1sev')).toMatch(/lowercase/i)
    expect(validateCommandArgumentName('')).toMatch(/required/i)
    expect(validateCommandArgumentName('a'.repeat(33))).toMatch(/lowercase/i)
  })

  it('flags a duplicate argument name', () => {
    const args = [
      { name: 'sev', required: false },
      { name: 'sev', required: false },
    ]
    expect(commandArgumentError(args[0]!, 0, args)).toBeNull()
    expect(commandArgumentError(args[1]!, 1, args)).toMatch(/duplicate/i)
  })

  it('flags an over-long description', () => {
    const arg = { name: 'sev', description: 'a'.repeat(257), required: false }
    expect(commandArgumentError(arg, 0, [arg])).toMatch(/256/)
  })

  it(`limits to ${MAX_COMMAND_ARGUMENTS} arguments`, () => {
    const args = Array.from({ length: MAX_COMMAND_ARGUMENTS + 1 }, (_, i) => ({
      name: `arg${i}`,
      required: false,
    }))
    expect(validateCommandArgumentCount(args)).toMatch(/at most 10/i)
    expect(validateCommandArgumentCount(args.slice(0, MAX_COMMAND_ARGUMENTS))).toBeNull()
  })
})

describe('findUndeclaredPlaceholders', () => {
  it('returns placeholders missing from the declared arguments', () => {
    const undeclared = findUndeclaredPlaceholders('Severity is {{severity}}, owner {{owner}}.', [
      { name: 'severity', required: true },
    ])
    expect(undeclared).toEqual(['owner'])
  })

  it('returns nothing when every placeholder is declared', () => {
    const undeclared = findUndeclaredPlaceholders('{{a}} and {{b}}', [
      { name: 'a', required: false },
      { name: 'b', required: false },
    ])
    expect(undeclared).toEqual([])
  })

  it('returns nothing when the template has no placeholders', () => {
    expect(findUndeclaredPlaceholders('plain text', [])).toEqual([])
  })
})
