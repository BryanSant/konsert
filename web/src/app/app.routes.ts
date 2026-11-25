import { Route, UrlMatchResult, UrlSegment, Routes } from '@angular/router';
import { resolveSidRoute, sidMatcher } from './core/routing/sid-links';

/** Phone widths get the watch-and-answer inbox; everything wider lands in Command. */
export const PHONE_QUERY = '(max-width: 767px)';

export function isPhoneWidth(): boolean {
  return typeof window !== 'undefined' && window.matchMedia?.(PHONE_QUERY).matches === true;
}

/**
 * Matches `/<mode>` and `/<mode>/<sid>` with one route, so moving between a mode's
 * overview and an item (a tab, a review card, a machine) keeps the component (and its
 * terminals) alive. The sid is a short user-facing id (t-…, c-…, m-…).
 */
export function modeMatcher(mode: string): (segments: UrlSegment[]) => UrlMatchResult | null {
  return (segments) => {
    if (segments[0]?.path !== mode || segments.length > 2) return null;
    const posParams: Record<string, UrlSegment> = {};
    if (segments[1]) posParams['sid'] = segments[1];
    return { consumed: segments, posParams };
  };
}

const modeShell = () => import('./modes/mode-shell.component').then((m) => m.ModeShellComponent);

export const MODES: ReadonlyArray<{ id: 'command' | 'work' | 'fleet' | 'wall'; key: string; label: string }> = [
  { id: 'command', key: '1', label: 'Command' },
  { id: 'work', key: '2', label: 'Work' },
  { id: 'fleet', key: '3', label: 'Fleet' },
  { id: 'wall', key: '4', label: 'Wall' },
];

const modeRoutes: Route[] = [
  {
    matcher: modeMatcher('command'),
    title: 'Command · Syre',
    loadComponent: () => import('./modes/command/command-mode.component').then((m) => m.CommandModeComponent),
  },
  {
    matcher: modeMatcher('work'),
    title: 'Work · Syre',
    loadComponent: () => import('./modes/work/work-mode.component').then((m) => m.WorkModeComponent),
  },
  {
    matcher: modeMatcher('fleet'),
    title: 'Fleet · Syre',
    loadComponent: () => import('./modes/fleet/fleet-mode.component').then((m) => m.FleetModeComponent),
  },
  {
    path: 'wall',
    title: 'Wall · Syre',
    loadComponent: () => import('./modes/wall/wall-mode.component').then((m) => m.WallModeComponent),
  },
];

export const routes: Routes = [
  // Sign-in links. Signed out, the app shows the pairing page for /pair/{code}; signed in
  // there is nothing to pair. /approve/{code} approves a `syre login` request.
  {
    path: 'pair/:code',
    loadComponent: () => import('./features/auth/pair-redirect.component').then((m) => m.PairRedirectComponent),
  },
  {
    path: 'approve/:code',
    title: 'Approve sign-in · Syre',
    loadComponent: () => import('./features/auth/approve-login.component').then((m) => m.ApproveLoginComponent),
  },
  // Mode-agnostic short links (`/<sid>`) redirect to the right mode URL.
  {
    matcher: sidMatcher,
    redirectTo: (info) => {
      const sid = info.params['sid'] ?? info.url[0]?.path ?? '';
      return resolveSidRoute(sid, { phone: isPhoneWidth() }) ?? '/';
    },
  },
  {
    path: '',
    loadComponent: () => import('./modes/workspace-root.component').then((m) => m.WorkspaceRootComponent),
    children: [
      { path: '', pathMatch: 'full', redirectTo: () => (isPhoneWidth() ? 'inbox' : 'command') },
      {
        path: 'inbox',
        title: 'Needs you · Syre',
        loadComponent: () => import('./modes/mobile/mobile-inbox.component').then((m) => m.MobileInboxComponent),
      },
      {
        path: 'inbox/fleet',
        title: 'Fleet · Syre',
        loadComponent: () => import('./modes/mobile/mobile-fleet.component').then((m) => m.MobileFleetComponent),
      },
      {
        path: 'inbox/:sid',
        title: 'Session · Syre',
        loadComponent: () => import('./modes/mobile/mobile-session.component').then((m) => m.MobileSessionComponent),
      },
      { path: '', loadComponent: modeShell, children: modeRoutes },
    ],
  },
  { path: '**', redirectTo: '' },
];
