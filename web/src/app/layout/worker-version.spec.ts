import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Worker } from '../core/models/konsert.models';
import { isVersionMismatch, normalizeVersion } from '../core/models/version';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { FleetSidebarComponent } from './fleet-sidebar.component';
import { MachineDetailModalComponent } from './machine-detail-modal.component';

function worker(id: string, version?: string): Worker {
  return {
    id,
    name: `node-${id}`,
    host_group: 'lab',
    os: 'linux',
    arch: 'amd64',
    isolation: 'host',
    permission_mode: 'full_access',
    agents: ['claude'],
    tags: {},
    max_concurrent: 2,
    status: 'online',
    last_seen_at: new Date().toISOString(),
    version,
  } as Worker;
}

describe('version helpers', () => {
  it('treats empty and dev versions as unknown', () => {
    expect(normalizeVersion('')).toBe('');
    expect(normalizeVersion(undefined)).toBe('');
    expect(normalizeVersion(' dev ')).toBe('');
    expect(normalizeVersion('v1.2.0')).toBe('1.2.0');
  });

  it('flags only known, differing versions', () => {
    expect(isVersionMismatch('1.2.0', '1.1.0')).toBe(true);
    expect(isVersionMismatch('v1.2.0', '1.2.0')).toBe(false);
    expect(isVersionMismatch('1.2.0', 'dev')).toBe(false);
    expect(isVersionMismatch('1.2.0', '')).toBe(false);
    expect(isVersionMismatch('dev', '1.1.0')).toBe(false);
    expect(isVersionMismatch('', '1.1.0')).toBe(false);
  });
});

describe('worker version UI', () => {
  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: { setUnauthorizedHandler: vi.fn(), deleteWorker: vi.fn() },
        },
      ],
    });
  });

  afterEach(() => TestBed.resetTestingModule());

  describe('FleetSidebarComponent machines list', () => {
    function render(hubVersion: string, workers: Worker[]) {
      const fleet = TestBed.inject(FleetStore);
      fleet.controllerVersion.set(hubVersion);
      fleet.workers.set(workers);
      const fixture = TestBed.createComponent(FleetSidebarComponent);
      fixture.componentInstance.navMode.set('machines');
      fixture.detectChanges();
      return fixture.nativeElement as HTMLElement;
    }

    it('shows an accessible mismatch badge only for mismatched workers', () => {
      const root = render('1.5.0', [
        worker('old', '1.4.0'),
        worker('same', 'v1.5.0'),
        worker('dev', 'dev'),
        worker('none'),
      ]);
      const badges = root.querySelectorAll('[data-testid="version-mismatch-badge"]');
      expect(badges.length).toBe(1);
      const badge = badges[0] as HTMLElement;
      expect(badge.getAttribute('aria-label')).toContain('node-old');
      expect(badge.getAttribute('aria-label')).toContain('1.4.0');
      expect(badge.getAttribute('aria-label')).toContain('1.5.0');
      // No native title: the ksTooltip repeats the aria-label, so it isn't linked twice.
      expect(badge.hasAttribute('title')).toBe(false);
      expect(badge.hasAttribute('aria-describedby')).toBe(false);
      expect(badge.getAttribute('role')).toBe('img');
    });

    it('never flags workers when the hub is a dev build', () => {
      const root = render('dev', [worker('a', '1.4.0'), worker('b', '1.5.0')]);
      expect(root.querySelectorAll('[data-testid="version-mismatch-badge"]').length).toBe(0);
    });
  });

  describe('MachineDetailModalComponent', () => {
    function render(hubVersion: string, w: Worker) {
      TestBed.inject(FleetStore).controllerVersion.set(hubVersion);
      const fixture = TestBed.createComponent(MachineDetailModalComponent);
      fixture.componentRef.setInput('worker', w);
      fixture.detectChanges();
      return fixture.nativeElement as HTMLElement;
    }

    it('shows the worker version and a mismatch note', () => {
      const root = render('1.5.0', worker('x', '1.4.0'));
      const row = root.querySelector('[data-testid="worker-version"]') as HTMLElement;
      expect(row.textContent).toContain('konsertd version');
      expect(row.textContent).toContain('1.4.0');
      const note = root.querySelector('[data-testid="worker-version-mismatch"]') as HTMLElement;
      expect(note.textContent).toContain('Differs from hub 1.5.0');
      expect(note.hasAttribute('title')).toBe(false);
      const tip = document.getElementById(note.getAttribute('aria-describedby') ?? '');
      expect(tip?.textContent).toContain('1.4.0');
    });

    it('shows matching versions without a warning', () => {
      const root = render('1.5.0', worker('x', '1.5.0'));
      expect(root.querySelector('[data-testid="worker-version"]')?.textContent).toContain('1.5.0');
      expect(root.querySelector('[data-testid="worker-version-mismatch"]')).toBeNull();
    });

    it('labels a missing version as unknown and dev as-is, without a warning', () => {
      let root = render('1.5.0', worker('x'));
      expect(root.querySelector('[data-testid="worker-version"]')?.textContent).toContain(
        'unknown',
      );
      expect(root.querySelector('[data-testid="worker-version-mismatch"]')).toBeNull();
      TestBed.resetTestingModule();
      root = render('1.5.0', worker('y', 'dev'));
      expect(root.querySelector('[data-testid="worker-version"]')?.textContent).toContain('dev');
      expect(root.querySelector('[data-testid="worker-version-mismatch"]')).toBeNull();
    });
  });
});
