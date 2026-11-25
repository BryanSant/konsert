import { Component } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { provideRouter, Router, UrlSegment } from '@angular/router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { isPhoneWidth } from '../../app.routes';
import { parseSid, resolveSidRoute, SID_PREFIXES, sidMatcher, sidUrl } from './sid-links';

@Component({ standalone: true, template: '' })
class DummyComponent {}

describe('sid-links', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  describe('parseSid', () => {
    it('parses every known sid prefix from internal/ids', () => {
      for (const prefix of SID_PREFIXES) {
        expect(parseSid(`${prefix}-7qk3m`)).toEqual({ prefix, suffix: '7qk3m' });
      }
    });

    it('rejects unknown prefixes, mode names, and malformed values', () => {
      for (const bad of [
        '',
        'command',
        'fleet',
        'work',
        'wall',
        'inbox',
        'pair',
        'approve',
        't-',
        '-abcde',
        'x-7qk3m',
        'foo-7qk3m',
        't-7qk_3m',
        'T-7qk3m',
        't-7qk3m/extra',
      ]) {
        expect(parseSid(bad), bad).toBeNull();
      }
    });
  });

  describe('resolveSidRoute', () => {
    it('routes tasks, chats, and machines on desktop width', () => {
      expect(resolveSidRoute('t-7qk3m', { phone: false })).toBe('/command/t-7qk3m');
      expect(resolveSidRoute('c-yvp2f', { phone: false })).toBe('/command/c-yvp2f');
      expect(resolveSidRoute('m-k4n8p', { phone: false })).toBe('/fleet/m-k4n8p');
    });

    it('routes tasks and chats to inbox/<sid> on phone width, and machines to fleet/<sid>', () => {
      expect(resolveSidRoute('t-7qk3m', { phone: true })).toBe('/inbox/t-7qk3m');
      expect(resolveSidRoute('c-yvp2f', { phone: true })).toBe('/inbox/c-yvp2f');
      expect(resolveSidRoute('m-k4n8p', { phone: true })).toBe('/fleet/m-k4n8p');
    });

    it('routes every other known sid prefix to an appropriate mode', () => {
      expect(resolveSidRoute('s-7qk3m', { phone: false })).toBe('/fleet');
      expect(resolveSidRoute('mc-7qk3m', { phone: false })).toBe('/fleet');
      expect(resolveSidRoute('s-7qk3m', { phone: true })).toBe('/fleet');
      expect(resolveSidRoute('mc-7qk3m', { phone: true })).toBe('/fleet');

      for (const prefix of ['rv', 'p', 'r', 'ws', 'wt'] as const) {
        expect(resolveSidRoute(`${prefix}-7qk3m`, { phone: false })).toBe('/work');
        expect(resolveSidRoute(`${prefix}-7qk3m`, { phone: true })).toBe('/inbox');
      }

      for (const prefix of ['q', 'rq', 'o', 'v', 'h', 'dv', 'pc'] as const) {
        expect(resolveSidRoute(`${prefix}-7qk3m`, { phone: false })).toBe('/command');
        expect(resolveSidRoute(`${prefix}-7qk3m`, { phone: true })).toBe('/inbox');
      }
    });

    it('returns null for unrecognized strings', () => {
      expect(resolveSidRoute('command')).toBeNull();
      expect(resolveSidRoute('zz-7qk3m')).toBeNull();
    });
  });

  describe('sidMatcher', () => {
    const segs = (...parts: string[]) => parts.map((p) => new UrlSegment(p, {}));

    it('matches a single valid sid segment and exposes sid param', () => {
      const res = sidMatcher(segs('t-7qk3m'));
      expect(res?.consumed.map((s) => s.path)).toEqual(['t-7qk3m']);
      expect(res?.posParams?.['sid']?.path).toBe('t-7qk3m');
    });

    it('rejects multi-segment paths and non-sid paths', () => {
      expect(sidMatcher(segs('t-7qk3m', 'extra'))).toBeNull();
      expect(sidMatcher(segs('command'))).toBeNull();
      expect(sidMatcher(segs('foo-12345'))).toBeNull();
      expect(sidMatcher([])).toBeNull();
    });
  });

  describe('sidUrl', () => {
    it('builds origin + / + sid without double slashes', () => {
      expect(sidUrl('t-7qk3m', 'https://syre.example')).toBe('https://syre.example/t-7qk3m');
      expect(sidUrl('m-k4n8p', 'http://127.0.0.1:8420/')).toBe('http://127.0.0.1:8420/m-k4n8p');
    });
  });

  describe('Angular Router short-link redirect', () => {
    const origMatchMedia = window.matchMedia;

    afterEach(() => {
      Object.defineProperty(window, 'matchMedia', {
        writable: true,
        configurable: true,
        value: origMatchMedia,
      });
    });

    function setupRouter(phone: boolean): Router {
      Object.defineProperty(window, 'matchMedia', {
        writable: true,
        configurable: true,
        value: (query: string) =>
          ({
            matches: phone,
            media: query,
            onchange: null,
            addListener: vi.fn(),
            removeListener: vi.fn(),
            addEventListener: vi.fn(),
            removeEventListener: vi.fn(),
            dispatchEvent: vi.fn(),
          }) as unknown as MediaQueryList,
      });
      TestBed.resetTestingModule();
      TestBed.configureTestingModule({
        providers: [
          provideRouter([
            {
              matcher: sidMatcher,
              redirectTo: (info) => {
                const sid = info.params['sid'] ?? info.url[0]?.path ?? '';
                return resolveSidRoute(sid, { phone: isPhoneWidth() }) ?? '/';
              },
            },
            { path: 'command/:sid', component: DummyComponent },
            { path: 'command', component: DummyComponent },
            { path: 'fleet/:sid', component: DummyComponent },
            { path: 'fleet', component: DummyComponent },
            { path: 'work', component: DummyComponent },
            { path: 'inbox/:sid', component: DummyComponent },
            { path: 'inbox', component: DummyComponent },
            { path: '**', redirectTo: 'command' },
          ]),
        ],
      });
      return TestBed.inject(Router);
    }

    it('redirects /<sid> to the right mode URL on desktop', async () => {
      const router = setupRouter(false);

      await router.navigateByUrl('/t-7qk3m');
      expect(router.url).toBe('/command/t-7qk3m');

      await router.navigateByUrl('/c-yvp2f');
      expect(router.url).toBe('/command/c-yvp2f');

      await router.navigateByUrl('/m-k4n8p');
      expect(router.url).toBe('/fleet/m-k4n8p');

      await router.navigateByUrl('/rv-ab12c');
      expect(router.url).toBe('/work');
    });

    it('redirects /<sid> to /inbox/<sid> for tasks and chats on phone width', async () => {
      const router = setupRouter(true);

      await router.navigateByUrl('/t-7qk3m');
      expect(router.url).toBe('/inbox/t-7qk3m');

      await router.navigateByUrl('/c-yvp2f');
      expect(router.url).toBe('/inbox/c-yvp2f');

      await router.navigateByUrl('/m-k4n8p');
      expect(router.url).toBe('/fleet/m-k4n8p');
    });
  });
});
