import { signal, Type } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Task, Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { AuditLogComponent } from './audit-log.component';
import { FleetSidebarComponent } from './fleet-sidebar.component';
import { SettingsModalComponent } from './settings-modal.component';
import { StatusFooterComponent } from './status-footer.component';
import { TabStripComponent } from './tab-strip.component';
import { TelemetryDrawerComponent } from './telemetry-drawer.component';

const worker: Worker = {
  id: 'w1',
  name: 'studio',
  host_group: 'studio',
  os: 'darwin',
  arch: 'arm64',
  isolation: 'host',
  permission_mode: 'full_access',
  agents: ['claude', 'shell'],
  tags: {},
  max_concurrent: 2,
  status: 'online',
  last_seen_at: new Date().toISOString(),
};

const task = { id: 't1', worker_id: 'w1', agent: 'claude', status: 'running', workdir: '/src/app', created_at: new Date().toISOString() } as Task;

/**
 * Density pass guard rails (KON-8A72G): the shell, inspector and modals use the shared
 * primitives, with no native title tooltips and no bordered form fields.
 */
describe('density pass primitives', () => {
  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: {
            setUnauthorizedHandler: vi.fn(),
            inspectGit: vi.fn(async () => ({ is_git_repo: false, files: [] })),
            getTaskEvents: vi.fn(async () => ({ events: [] })),
            listAudit: vi.fn(async () => ({ events: [] })),
            getAudit: vi.fn(async () => ({ events: [] })),
            listAuditEvents: vi.fn(async () => ({ events: [], has_more: false })),
            getSettings: vi.fn(async () => ({})),
            getProviderSettings: vi.fn(async () => ({})),
            listAgentAccounts: vi.fn(async () => ({ accounts: [] })),
          },
        },
        { provide: AuthService, useValue: { provider: signal(''), user: signal('tester'), authRequired: signal(false) } },
      ],
    });
    const fleet = TestBed.inject(FleetStore);
    fleet.upsertWorker(worker);
    fleet.upsertTask(task);
    TestBed.inject(TabsStore).openTaskTab(task, 'studio', false);
  });

  afterEach(() => {
    TestBed.resetTestingModule();
    document.querySelectorAll('.ks-tooltip').forEach((n) => n.remove());
  });

  const surfaces: Array<[string, Type<unknown>, Record<string, unknown>?]> = [
    ['fleet sidebar', FleetSidebarComponent],
    ['tab strip', TabStripComponent],
    ['status footer', StatusFooterComponent],
    ['telemetry drawer (activity)', TelemetryDrawerComponent, { panel: 'activity' }],
    ['telemetry drawer (checks)', TelemetryDrawerComponent, { panel: 'checks' }],
    ['audit log', AuditLogComponent],
    ['settings modal', SettingsModalComponent],
  ];

  for (const [name, cmp, inputs] of surfaces) {
    it(`${name}: no native title, icon buttons are labelled ks-icon-btn, fields are ks-field`, async () => {
      const fixture = TestBed.createComponent(cmp);
      for (const [k, v] of Object.entries(inputs ?? {})) fixture.componentRef.setInput(k, v);
      document.body.appendChild(fixture.nativeElement);
      fixture.detectChanges();
      await fixture.whenStable();
      const root = fixture.nativeElement as HTMLElement;

      expect(Array.from(root.querySelectorAll('[title]')).map((e) => e.outerHTML.slice(0, 80))).toEqual([]);

      const iconOnly = Array.from(root.querySelectorAll<HTMLButtonElement>('button')).filter(
        (b) => b.children.length === 1 && b.children[0].tagName === 'APP-ICON' && !b.textContent?.trim(),
      );
      expect(root.children.length).toBeGreaterThan(0);
      if (cmp === FleetSidebarComponent || cmp === TabStripComponent) expect(iconOnly.length).toBeGreaterThan(0);
      for (const b of iconOnly) {
        expect(b.getAttribute('aria-label'), b.outerHTML.slice(0, 120)).toBeTruthy();
        expect(b.classList.contains('ks-icon-btn'), b.outerHTML.slice(0, 160)).toBe(true);
      }

      const fields = Array.from(root.querySelectorAll<HTMLElement>('input, select, textarea')).filter(
        (f) => !['checkbox', 'radio', 'range', 'hidden', 'file'].includes((f as HTMLInputElement).type),
      );
      for (const f of fields) {
        if (f.closest('[role="combobox"]') || f.classList.contains('ks-sr-only')) continue;
        expect(f.className, f.outerHTML.slice(0, 160)).not.toMatch(/\bborder-\[#/);
      }
      fixture.nativeElement.remove();
    });
  }
});
