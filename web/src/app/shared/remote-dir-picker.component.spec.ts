import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FSListResult } from '../core/models/syre.models';
import { ApiClientService } from '../core/services/api-client.service';
import { RemoteDirPickerComponent } from './remote-dir-picker.component';

describe('RemoteDirPickerComponent', () => {
  let pending: Array<{ path: string; workerId?: string; resolve: (r: FSListResult) => void }>;
  let listRemoteFS: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    pending = [];
    listRemoteFS = vi.fn(
      (p: { path: string; workerId?: string }) =>
        new Promise<FSListResult>((resolve) =>
          pending.push({ path: p.path, workerId: p.workerId, resolve }),
        ),
    );
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { listRemoteFS } }],
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  function listing(dir: string): FSListResult {
    return {
      current_dir: dir,
      parent_dir: '/',
      entries: [{ name: 'x', path: `${dir}/x`, is_dir: true }],
    } as unknown as FSListResult;
  }

  function create(workerId = 'w1') {
    const fixture = TestBed.createComponent(RemoteDirPickerComponent);
    fixture.componentRef.setInput('workerId', workerId);
    fixture.detectChanges();
    TestBed.tick();
    return { fixture, cmp: fixture.componentInstance };
  }

  it('issues exactly one request per browse navigation', () => {
    const { cmp } = create();
    cmp.toggleBrowse();
    TestBed.tick();
    expect(listRemoteFS).toHaveBeenCalledTimes(1);

    cmp.navigateBrowse('/srv');
    TestBed.tick();
    expect(listRemoteFS).toHaveBeenCalledTimes(2);
    expect(pending[1].path).toBe('/srv');
  });

  it('ignores an older browse response that resolves after a newer one', async () => {
    const { cmp } = create();
    cmp.toggleBrowse();
    cmp.navigateBrowse('/new');
    pending[1].resolve(listing('/new'));
    await vi.waitFor(() => expect(cmp.browseCurrentDir()).toBe('/new'));
    pending[0].resolve(listing('/old'));
    await Promise.resolve();
    await Promise.resolve();
    expect(cmp.browseCurrentDir()).toBe('/new');
    expect(cmp.loadingBrowse()).toBe(false);
  });

  it('debounces typing into a single suggestion request and never browses per keystroke', () => {
    const { cmp } = create();
    cmp.toggleBrowse();
    listRemoteFS.mockClear();
    for (const v of ['/', '/s', '/sr', '/src']) {
      cmp.onInputChange(v);
      TestBed.tick();
      vi.advanceTimersByTime(50);
    }
    expect(listRemoteFS).not.toHaveBeenCalled();
    vi.advanceTimersByTime(200);
    expect(listRemoteFS).toHaveBeenCalledTimes(1);
    expect(listRemoteFS.mock.calls[0][0]).toMatchObject({ path: '/src' });
  });

  it('reloads the browse view once when the target machine changes', () => {
    const { fixture } = create('w1');
    fixture.componentInstance.toggleBrowse();
    listRemoteFS.mockClear();
    fixture.componentRef.setInput('workerId', 'w2');
    TestBed.tick();
    expect(listRemoteFS).toHaveBeenCalledTimes(1);
    expect(listRemoteFS.mock.calls[0][0]).toMatchObject({ workerId: 'w2' });
  });
});
