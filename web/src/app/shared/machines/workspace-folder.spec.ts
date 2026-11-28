import { describe, expect, it } from 'vitest';
import {
  WorkspaceFolders,
  coverageText,
  displayFolder,
  normalizeFolder,
  osSpecificWarning,
} from './workspace-folder';

describe('workspace folder', () => {
  it('normalizes like the hub and refuses what leaves home', () => {
    expect(normalizeFolder('~/src/')).toEqual({ path: 'src' });
    expect(normalizeFolder('~\\source\\repos')).toEqual({ path: 'source/repos' });
    expect(normalizeFolder(' code//work/./ ')).toEqual({ path: 'code/work' });
    expect(normalizeFolder('~')).toEqual({ path: '' });
    for (const bad of ['/home/me/src', 'C:\\src', '~/../etc', 'a:b', '\\\\server\\share']) {
      expect(normalizeFolder(bad).error, bad).toBeTruthy();
    }
  });

  it('renders per OS: macOS, Linux and Windows', () => {
    expect(displayFolder('source/repos', 'darwin')).toBe('~/source/repos');
    expect(displayFolder('source/repos', 'linux')).toBe('~/source/repos');
    expect(displayFolder('source/repos', 'windows')).toBe('~\\source\\repos');
    expect(displayFolder('', 'windows')).toBe('~');
  });

  it('warns about folders tied to one OS', () => {
    expect(osSpecificWarning('AppData/Roaming/x')).toBe(
      '~/AppData/Roaming/x is a Windows folder: it will be created as ~/AppData/Roaming/x on macOS and Linux.',
    );
    expect(osSpecificWarning('Library/code')).toContain('on Linux and Windows');
    expect(osSpecificWarning('src')).toBe('');
  });

  it('says how many machines have it and where it will be created', () => {
    const folders: WorkspaceFolders = {
      suggestions: [{ path: 'src', machine_ids: ['a', 'b'], count: 2, total: 3, repos: 4 }],
      total: 3,
      saved: '',
      effective: 'src',
    };
    const names = { a: 'box-a', b: 'box-b', c: 'daybreak-wsl' };
    expect(coverageText('src', folders, names)).toBe(
      'on 2 of 3 machines · will be created on daybreak-wsl',
    );
    expect(coverageText('scr', folders, names)).toBe(
      'on 0 of 3 machines · will be created on box-a, box-b, daybreak-wsl',
    );
    expect(coverageText('', folders, names)).toBe('');
  });
});
