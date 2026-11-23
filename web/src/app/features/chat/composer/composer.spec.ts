import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  FIXTURE_WORKER_ID,
  claude2Account,
  claudeAccount,
  codexLoggedOut,
  cursorAccount,
} from '../../../../testing/agent-chat.fixtures';
import { AgentAccount } from '../../../core/models/agent-chat.models';
import { ApiClientService } from '../../../core/services/api-client.service';
import { resetTooltipWarmState } from '../../../shared/tooltip.directive';
import { ChatComposerComponent, ComposerSubmit } from './chat-composer.component';
import { joinDictation, speechErrorMessage, transcriptOf } from './speech-dictation';
import {
  ComposerSelection,
  approvalTone,
  defaultApproval,
  effortsFor,
  emptySelection,
  filterModels,
  modelTriggerText,
  normalizeSelection,
  pickDefaultAccount,
  selectModel,
} from './composer-logic';

const SANDBOX_REASON = 'This machine runs konsertd with --sandbox, which refuses full-access modes.';
/** Claude on a --sandbox worker: full access is disabled with the reason. */
function sandboxedClaude(over: Partial<AgentAccount> = {}): AgentAccount {
  const base = claudeAccount();
  return {
    ...base,
    approval_modes: base.approval_modes!.map((m) => (m.risk >= 4 ? { ...m, disabled: SANDBOX_REASON } : m)),
    ...over,
  };
}

describe('composer logic', () => {
  const claude = claudeAccount();

  it('never auto-selects a disabled approval mode', () => {
    const sel = { ...emptySelection(), approvalMode: 'bypassPermissions' };
    // A carried-over full-access choice falls back to the default on a sandboxed worker.
    expect(normalizeSelection(sel, sandboxedClaude()).approvalMode).toBe('default');
    // Even when the account's default (or the first mode) is the disabled one.
    const odd = sandboxedClaude({ default_approval_mode: 'bypassPermissions' });
    expect(defaultApproval(odd)).toBe('plan');
    const first = sandboxedClaude();
    first.approval_modes = [...first.approval_modes!].reverse();
    expect(defaultApproval({ ...first, default_approval_mode: '' })).toBe('auto');
  });

  it('filters models by name, id and description, all terms', () => {
    const models = claude.models!;
    expect(filterModels(models, '').map((m) => m.id)).toEqual(['opus', 'sonnet', 'haiku']);
    expect(filterModels(models, 'son').map((m) => m.id)).toEqual(['sonnet']);
    expect(filterModels(models, 'fastest').map((m) => m.id)).toEqual(['haiku']);
    expect(filterModels(models, 'most complex').map((m) => m.id)).toEqual(['opus']);
    expect(filterModels(models, 'gpt')).toEqual([]);
  });

  it('shows only the selected model efforts (none for haiku)', () => {
    expect(effortsFor(claude.models![0]).map((e) => e.short)).toEqual(['Low', 'Med', 'High', 'XHigh', 'Max']);
    expect(effortsFor(claude.models![1]).map((e) => e.short)).toEqual(['Low', 'Med', 'High', 'XHigh']);
    expect(effortsFor(claude.models![2])).toEqual([]);
  });

  it('switching accounts re-validates model, effort and approval against the new account', () => {
    let sel: ComposerSelection = normalizeSelection({ ...emptySelection(), workerId: FIXTURE_WORKER_ID }, claude);
    expect(sel).toMatchObject({ accountId: 'claude', model: 'sonnet', effort: 'medium', approvalMode: 'default' });
    sel = selectModel(sel, claude, 'opus');
    sel = { ...sel, effort: 'max', approvalMode: 'bypassPermissions' };
    // Cursor has neither opus nor efforts, and other approval ids.
    const cur = normalizeSelection(sel, cursorAccount());
    expect(cur).toMatchObject({ accountId: 'cursor', model: 'auto', effort: '', approvalMode: 'ask' });
    // Claude 2 shares the model list: keep the choices.
    const c2 = normalizeSelection(sel, claude2Account());
    expect(c2).toMatchObject({ accountId: 'claude2', model: 'opus', effort: 'max', approvalMode: 'bypassPermissions' });
  });

  it('selecting a model keeps a supported effort and falls back otherwise', () => {
    const sel = { ...normalizeSelection(emptySelection(), claude), model: 'opus', effort: 'max' };
    expect(selectModel(sel, claude, 'sonnet').effort).toBe('medium'); // sonnet has no max
    expect(selectModel({ ...sel, effort: 'high' }, claude, 'sonnet').effort).toBe('high');
    expect(selectModel(sel, claude, 'haiku').effort).toBe('');
  });

  it('picks a healthy default account, honoring preferences', () => {
    const list = [claudeAccount(), claude2Account(), codexLoggedOut(), cursorAccount()];
    expect(pickDefaultAccount(list)?.id).toBe('claude');
    expect(pickDefaultAccount(list, { agent: 'cursor' })?.id).toBe('cursor');
    expect(pickDefaultAccount(list, { accountId: 'claude2' })?.id).toBe('claude2');
    expect(pickDefaultAccount([claude2Account()])?.id).toBe('claude2');
  });

  it('prefers Claude over an alphabetically earlier agent when nothing was chosen', () => {
    // Seen live: a new chat defaulted to Antigravity ("agy" sorts first).
    const agy = { ...cursorAccount(), id: 'agy', agent: 'agy', label: 'Antigravity' };
    expect(pickDefaultAccount([agy, cursorAccount(), claudeAccount()])?.id).toBe('claude');
  });

  it('formats the trigger and tones full access amber', () => {
    expect(modelTriggerText(claude.models![1], 'medium')).toEqual({ label: 'Sonnet 5.5 Med', tooltip: 'Sonnet 5.5 · Medium' });
    expect(modelTriggerText(claude.models![2], '')).toEqual({ label: 'Haiku 5', tooltip: 'Haiku 5' });
    expect(approvalTone(claude.approval_modes![4])).toBe('amber');
    expect(approvalTone(claude.approval_modes![1])).toBe('default');
  });
});

@Component({
  standalone: true,
  imports: [ChatComposerComponent],
  template: `
    <ks-chat-composer
      [accounts]="accounts()"
      [(selection)]="selection"
      [busy]="busy()"
      (submitted)="sent.push($event)"
      (controlChange)="changes.push($event)"
      (stop)="stops = stops + 1"
    />
  `,
})
class Host {
  readonly accounts = signal<AgentAccount[]>([claudeAccount(), claude2Account(), codexLoggedOut(), cursorAccount()]);
  readonly selection = signal<ComposerSelection>(
    normalizeSelection({ ...emptySelection(), workerId: FIXTURE_WORKER_ID }, claudeAccount()),
  );
  readonly busy = signal(false);
  sent: ComposerSubmit[] = [];
  changes: Array<Partial<ComposerSelection>> = [];
  stops = 0;
}

describe('ChatComposerComponent', () => {
  beforeEach(() => {
    resetTooltipWarmState();
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { uploadAttachment: vi.fn(), setUnauthorizedHandler: vi.fn() } }],
    });
  });
  afterEach(() => {
    TestBed.resetTestingModule();
    document.querySelectorAll('.ks-tooltip').forEach((n) => n.remove());
  });

  async function setup() {
    const fixture = TestBed.createComponent(Host);
    document.body.appendChild(fixture.nativeElement);
    fixture.detectChanges();
    await fixture.whenStable();
    const el = fixture.nativeElement as HTMLElement;
    const render = async () => {
      fixture.detectChanges();
      await fixture.whenStable();
    };
    const $ = <T extends Element = HTMLElement>(sel: string) => el.querySelector(sel) as T;
    return { fixture, el, host: fixture.componentInstance, render, $ };
  }

  it('lays out the prompt, control row and context bar with no hard-coded models', async () => {
    const { el, $ } = await setup();
    expect(($('textarea') as HTMLTextAreaElement).placeholder).toBe('Ask anything, @ to mention files, / for commands');
    expect($('[aria-label="Attach file"]')).toBeTruthy();
    expect($('[data-testid="approval-chip"]').textContent).toContain('Ask for approval');
    expect($('[data-testid="model-trigger"]').textContent?.trim()).toBe('Sonnet 5.5 Med');
    expect($('[data-testid="account-chip"]').textContent).toContain('Claude');
    expect($('[data-testid="context-machine"]')).toBeTruthy();
    expect($('[aria-label="Dictate"]')).toBeNull(); // jsdom has no Web Speech API: hidden
    expect(el.textContent).not.toMatch(/gpt-/);
  });

  it('lists the approval modes of the account, marks the current one and applies a choice', async () => {
    const { host, render, $, el } = await setup();
    $<HTMLButtonElement>('[data-testid="approval-chip"]').click();
    await render();
    const items = Array.from(el.querySelectorAll('[role="menuitemradio"][data-mode]'));
    expect(items.map((i) => i.getAttribute('data-mode'))).toEqual(['plan', 'default', 'acceptEdits', 'auto', 'bypassPermissions']);
    expect(el.querySelector('[data-mode="default"]')!.getAttribute('aria-checked')).toBe('true');
    expect(el.querySelector('[data-mode="auto"]')!.textContent).toContain('Reviews approval requests for you');
    (el.querySelector('[data-mode="bypassPermissions"]') as HTMLButtonElement).click();
    await render();
    expect(host.selection().approvalMode).toBe('bypassPermissions');
    expect(host.changes.at(-1)).toEqual({ approvalMode: 'bypassPermissions' });
    expect($('[data-testid="approval-chip"]').classList.contains('ks-chip-amber')).toBe(true);
  });

  it('filters models with search and shows the effort row only for models with efforts', async () => {
    const { host, render, $, el } = await setup();
    $<HTMLButtonElement>('[data-testid="model-trigger"]').click();
    await render();
    const search = $<HTMLInputElement>('[data-testid="model-search"]');
    expect(document.activeElement).toBe(search);
    expect(el.querySelectorAll('[role="option"]').length).toBe(3);
    expect(el.querySelector('[data-model="sonnet"]')!.getAttribute('aria-selected')).toBe('true');
    expect(Array.from(el.querySelectorAll('[data-effort]')).map((b) => b.textContent?.trim())).toEqual(['Low', 'Med', 'High', 'XHigh']);

    search.value = 'haiku';
    search.dispatchEvent(new Event('input'));
    await render();
    expect(Array.from(el.querySelectorAll('[role="option"]')).map((o) => o.getAttribute('data-model'))).toEqual(['haiku']);
    search.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await render();
    expect(host.selection()).toMatchObject({ model: 'haiku', effort: '' });
    // Haiku has no efforts: the popover closes and the trigger has no effort suffix.
    expect($('[data-testid="effort-row"]')).toBeNull();
    expect($('[data-testid="model-trigger"]').textContent?.trim()).toBe('Haiku 5');
  });

  it('picks an effort in the segment and moves with the arrow keys', async () => {
    const { host, render, $, el } = await setup();
    $<HTMLButtonElement>('[data-testid="model-trigger"]').click();
    await render();
    (el.querySelector('[data-effort="high"]') as HTMLButtonElement).click();
    await render();
    expect(host.selection().effort).toBe('high');
    el.querySelector('[role="radiogroup"]')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
    await render();
    expect(host.selection().effort).toBe('xhigh');
    expect(host.changes.at(-1)).toEqual({ effort: 'xhigh' });
  });

  it('switching account changes the model list and approval modes', async () => {
    const { host, render, $, el } = await setup();
    $<HTMLButtonElement>('[data-testid="account-chip"]').click();
    await render();
    const rows = Array.from(el.querySelectorAll('[data-account]')).map((r) => r.getAttribute('data-account'));
    expect(rows).toEqual(['claude', 'claude2', 'codex', 'cursor']);
    (el.querySelector('[data-account="cursor"]') as HTMLButtonElement).click();
    await render();
    expect(host.selection()).toMatchObject({ accountId: 'cursor', model: 'auto', effort: '', approvalMode: 'ask' });
    expect(host.changes.at(-1)).toMatchObject({ accountId: 'cursor', model: 'auto' });
    expect($('[data-testid="model-trigger"]').textContent?.trim()).toBe('Auto');
    $<HTMLButtonElement>('[data-testid="approval-chip"]').click();
    await render();
    expect(Array.from(el.querySelectorAll('[data-mode]')).map((i) => i.getAttribute('data-mode'))).toEqual(['ask', 'force']);
  });

  it('warns before sending on an exhausted account and switches in one click, but still allows sending', async () => {
    const { host, render, $ } = await setup();
    host.selection.set(normalizeSelection(host.selection(), claude2Account()));
    await render();
    const warn = $('[data-testid="composer-warning"]');
    expect(warn.textContent).toMatch(/Claude 2 is out of weekly usage until/);
    expect(warn.textContent).toContain('Use Claude instead?');
    expect($('[data-testid="account-chip"]').textContent).toContain('Out');
    expect($('[data-testid="account-chip"]').textContent).toMatch(/agent account: Out of weekly usage until/);

    const ta = $<HTMLTextAreaElement>('textarea');
    ta.value = 'hi';
    ta.dispatchEvent(new Event('input'));
    await render();
    expect($<HTMLButtonElement>('[data-testid="send-button"]').disabled).toBe(false);

    $<HTMLButtonElement>('[data-testid="switch-account"]').click();
    await render();
    expect(host.selection().accountId).toBe('claude');
    expect($('[data-testid="composer-warning"]')).toBeNull();
  });

  it('blocks sending on a signed-out account with a clear reason', async () => {
    const { host, render, $ } = await setup();
    host.selection.set(normalizeSelection(host.selection(), codexLoggedOut()));
    await render();
    expect($('[data-testid="composer-warning"]').textContent).toContain('Codex needs you to sign in again.');
    expect($('[data-testid="account-chip"]').textContent).toContain('Re-login');
    const ta = $<HTMLTextAreaElement>('textarea');
    ta.value = 'hi';
    ta.dispatchEvent(new Event('input'));
    await render();
    expect($<HTMLButtonElement>('[data-testid="send-button"]').disabled).toBe(true);
  });

  it('sends on Enter (not Shift+Enter) and becomes Stop while busy', async () => {
    const { host, render, $ } = await setup();
    const ta = $<HTMLTextAreaElement>('textarea');
    ta.value = 'Run the tests';
    ta.dispatchEvent(new Event('input'));
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', shiftKey: true, bubbles: true }));
    expect(host.sent).toHaveLength(0);
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(host.sent).toEqual([{ text: 'Run the tests', attachments: [] }]);

    host.busy.set(true);
    await render();
    expect($('[data-testid="send-button"]')).toBeNull();
    $<HTMLButtonElement>('[data-testid="stop-button"]').click();
    expect(host.stops).toBe(1);
  });

  it('shows disabled approval modes greyed with the reason, never selects them, and skips them with the arrows', async () => {
    const { host, render, $, el } = await setup();
    host.accounts.set([sandboxedClaude()]);
    host.selection.set(normalizeSelection({ ...host.selection(), approvalMode: 'auto' }, sandboxedClaude()));
    await render();
    $<HTMLButtonElement>('[data-testid="approval-chip"]').click();
    await render();
    const full = el.querySelector<HTMLButtonElement>('[data-mode="bypassPermissions"]')!;
    expect(full.getAttribute('aria-disabled')).toBe('true');
    expect(full.textContent).toContain(SANDBOX_REASON);
    const reason = full.querySelector('[data-testid="approval-disabled-reason"]')!;
    expect(full.getAttribute('aria-describedby')).toBe(reason.id);
    // Enabled modes are not marked.
    expect(el.querySelector('[data-mode="auto"]')!.hasAttribute('aria-disabled')).toBe(false);

    full.click();
    await render();
    expect(host.selection().approvalMode).toBe('auto');
    expect(host.changes).toEqual([]);
    // The menu stays open after clicking a disabled item.
    expect(el.querySelector('[role="menu"]')).toBeTruthy();

    // Focus starts on the checked mode; ArrowDown wraps past the disabled last item.
    const auto = el.querySelector<HTMLButtonElement>('[data-mode="auto"]')!;
    expect(document.activeElement).toBe(auto);
    auto.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    expect(document.activeElement).toBe(el.querySelector('[data-mode="plan"]'));
    (document.activeElement as HTMLElement).dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true }));
    expect(document.activeElement).toBe(auto);
  });

  describe('speech-to-text', () => {
    class FakeRecognition {
      static last: FakeRecognition | null = null;
      continuous = false;
      interimResults = false;
      lang = '';
      onresult: ((ev: unknown) => void) | null = null;
      onerror: ((ev: { error: string }) => void) | null = null;
      onend: (() => void) | null = null;
      started = false;
      stopped = false;
      aborted = false;
      constructor() {
        FakeRecognition.last = this;
      }
      start() {
        this.started = true;
      }
      stop() {
        this.stopped = true;
      }
      abort() {
        this.aborted = true;
      }
      /** Emits results: [text, isFinal][]. */
      emit(parts: Array<[string, boolean]>) {
        const results = parts.map(([t, f]) => Object.assign([{ transcript: t }], { isFinal: f }));
        this.onresult?.({ resultIndex: 0, results });
      }
    }

    beforeEach(() => {
      FakeRecognition.last = null;
      vi.stubGlobal('webkitSpeechRecognition', FakeRecognition);
    });
    afterEach(() => vi.unstubAllGlobals());

    it('dictates into the prompt with interim results and stops on click', async () => {
      const { render, $ } = await setup();
      const ta = $<HTMLTextAreaElement>('textarea');
      ta.value = 'Please';
      ta.dispatchEvent(new Event('input'));
      const mic = $<HTMLButtonElement>('[data-testid="mic-button"]');
      expect(mic.getAttribute('aria-label')).toBe('Dictate');
      mic.click();
      await render();
      const rec = FakeRecognition.last!;
      expect(rec.started).toBe(true);
      expect(rec.continuous && rec.interimResults).toBe(true);
      expect(mic.getAttribute('aria-pressed')).toBe('true');
      expect(mic.getAttribute('aria-label')).toBe('Stop dictation');
      expect(mic.classList.contains('ks-mic-recording')).toBe(true);
      expect(document.body.textContent).toContain('Listening. Press Escape to cancel.');

      rec.emit([['run the', false]]);
      await render();
      expect(ta.value).toBe('Please run the');
      rec.emit([['run the tests', true], [' and fix', false]]);
      await render();
      expect(ta.value).toBe('Please run the tests and fix');

      mic.click();
      await render();
      expect(rec.stopped).toBe(true);
      rec.emit([['run the tests', true], [' and fix them', true]]);
      rec.onend!();
      await render();
      expect(ta.value).toBe('Please run the tests and fix them');
      expect(mic.getAttribute('aria-pressed')).toBe('false');
      expect($<HTMLButtonElement>('[data-testid="send-button"]').disabled).toBe(false);
    });

    it('Esc cancels dictation and restores the prompt', async () => {
      const { render, $, host } = await setup();
      const ta = $<HTMLTextAreaElement>('textarea');
      ta.value = 'Keep this';
      ta.dispatchEvent(new Event('input'));
      $<HTMLButtonElement>('[data-testid="mic-button"]').click();
      await render();
      const rec = FakeRecognition.last!;
      rec.emit([['oops', false]]);
      await render();
      expect(ta.value).toBe('Keep this oops');
      const esc = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
      ta.dispatchEvent(esc);
      await render();
      expect(rec.aborted).toBe(true);
      expect(esc.defaultPrevented).toBe(true);
      expect(ta.value).toBe('Keep this');
      expect($('[data-testid="mic-button"]').getAttribute('aria-pressed')).toBe('false');
      // Late events from the aborted session are ignored.
      rec.emit([['late', true]]);
      await render();
      expect(ta.value).toBe('Keep this');
      expect(host.sent).toEqual([]);
    });

    it('explains a denied microphone and lets the user dismiss it', async () => {
      const { render, $ } = await setup();
      $<HTMLButtonElement>('[data-testid="mic-button"]').click();
      await render();
      const rec = FakeRecognition.last!;
      rec.onerror!({ error: 'not-allowed' });
      rec.onend!();
      await render();
      expect($('[data-testid="speech-error"]').textContent).toContain('Microphone access is blocked');
      expect($('[data-testid="mic-button"]').getAttribute('aria-pressed')).toBe('false');
      $<HTMLButtonElement>('[data-testid="speech-error"] button').click();
      await render();
      expect($('[data-testid="speech-error"]')).toBeNull();
    });

    it('helpers join text, split final/interim and map errors', () => {
      expect(joinDictation('', ' hello')).toBe('hello');
      expect(joinDictation('a', 'b')).toBe('a b');
      expect(joinDictation('a\n', 'b')).toBe('a\nb');
      expect(joinDictation('a', '')).toBe('a');
      const results = [Object.assign([{ transcript: 'one ' }], { isFinal: true }), Object.assign([{ transcript: 'two' }], { isFinal: false })];
      expect(transcriptOf({ resultIndex: 1, results })).toEqual({ final: 'one ', interim: 'two' });
      expect(speechErrorMessage('aborted')).toBe('');
      expect(speechErrorMessage('audio-capture')).toContain('No microphone');
    });
  });
});
