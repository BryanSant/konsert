import { describe, expect, it } from 'vitest';
import { personLabel } from './people.store';

describe('personLabel', () => {
  it('shows the display name with the login on hover', () => {
    expect(personLabel('bsant', 'Bryan Sant')).toEqual({ text: 'Bryan Sant', tip: 'bsant' });
  });
  it('has no tooltip when the name is the login or unknown', () => {
    expect(personLabel('bsant', 'bsant')).toEqual({ text: 'bsant', tip: '' });
    expect(personLabel('bsant', undefined)).toEqual({ text: 'bsant', tip: '' });
    expect(personLabel('agent/claude', '')).toEqual({ text: 'agent/claude', tip: '' });
  });
});
