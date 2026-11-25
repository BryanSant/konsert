import { Injectable, inject } from '@angular/core';
import { Task, TaskStatus } from '../models/syre.models';
import { FleetStore, basenamePath } from '../state/fleet.store';
import { SessionNavService } from './session-nav.service';

@Injectable({ providedIn: 'root' })
export class NotificationService {
  private readonly fleet = inject(FleetStore);
  private readonly nav = inject(SessionNavService);

  private audioCtx: AudioContext | null = null;

  async requestDesktopPermission(): Promise<NotificationPermission | 'unsupported'> {
    if (typeof window === 'undefined' || !('Notification' in window)) {
      return 'unsupported';
    }
    if (Notification.permission === 'granted') {
      return 'granted';
    }
    try {
      return await Notification.requestPermission();
    } catch {
      return 'denied';
    }
  }

  notifyTaskTransition(prevStatus: TaskStatus | undefined, task: Task, workerName: string): void {
    if (!task || task.origin === 'scratch' || task.agent === 'shell') return;
    if (prevStatus === task.status) return;

    const isWaiting = task.status === 'waiting_input';
    const isDone = task.status === 'completed';
    const isFailed = task.status === 'failed';
    if (!isWaiting && !isDone && !isFailed) return;

    const pageUnfocused =
      typeof document !== 'undefined' && (document.hidden || (typeof document.hasFocus === 'function' && !document.hasFocus()));
    const isActiveTab = this.nav.isOpen(task.sid);

    const settings = this.fleet.settings();
    if (settings.notify_sound) {
      if (isWaiting) {
        this.playChime('waiting');
      } else if (isDone) {
        this.playChime('completed');
      } else if (isFailed) {
        this.playChime('failed');
      }
    }

    if (settings.notify_desktop && (pageUnfocused || !isActiveTab)) {
      this.showDesktopNotification(task, workerName, isWaiting ? 'waiting' : isDone ? 'completed' : 'failed');
    }
  }

  private playChime(kind: 'waiting' | 'completed' | 'failed'): void {
    if (typeof window === 'undefined') return;
    const AudioCtor =
      window.AudioContext || (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
    if (!AudioCtor) return;
    try {
      if (!this.audioCtx) {
        this.audioCtx = new AudioCtor();
      }
      const ctx = this.audioCtx;
      if (ctx.state === 'suspended') {
        void ctx.resume();
      }
      const now = ctx.currentTime;
      if (kind === 'waiting') {
        // Two-tone gentle attention prompt
        this.tone(ctx, 587.33, now, 0.11, 0.055);
        this.tone(ctx, 880.0, now + 0.12, 0.16, 0.05);
      } else if (kind === 'completed') {
        // Soft harmonic rising fifth
        this.tone(ctx, 523.25, now, 0.12, 0.045);
        this.tone(ctx, 659.25, now + 0.09, 0.16, 0.04);
      } else {
        // Subtle low descending error tone
        this.tone(ctx, 329.63, now, 0.14, 0.05);
        this.tone(ctx, 246.94, now + 0.13, 0.18, 0.045);
      }
    } catch {
      // Ignore browser autoplay restrictions when user hasn't interacted yet
    }
  }

  private tone(ctx: AudioContext, freq: number, start: number, dur: number, peakGain: number): void {
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.setValueAtTime(freq, start);
    gain.gain.setValueAtTime(0.0001, start);
    gain.gain.exponentialRampToValueAtTime(peakGain, start + 0.015);
    gain.gain.exponentialRampToValueAtTime(0.0001, start + dur);
    osc.connect(gain);
    gain.connect(ctx.destination);
    osc.start(start);
    osc.stop(start + dur + 0.02);
  }

  private showDesktopNotification(task: Task, workerName: string, kind: 'waiting' | 'completed' | 'failed'): void {
    if (typeof window === 'undefined' || !('Notification' in window)) return;
    if (Notification.permission !== 'granted') return;

    const dir = basenamePath(task.effective_dir || task.workdir) || task.agent;
    const title =
      kind === 'waiting'
        ? `${task.agent} needs your input`
        : kind === 'completed'
          ? `${task.agent} finished`
          : `${task.agent} failed`;
    const body = `${workerName || 'machine'} · ${dir}${task.summary ? ` — ${task.summary.slice(0, 90)}` : ''}`;

    try {
      const n = new Notification(title, {
        body,
        tag: `syre-task-${task.id}`,
        icon: 'logo.svg',
      });
      n.onclick = () => {
        try {
          window.focus();
        } catch {
          // ignore
        }
        this.nav.openTask(task);
        n.close();
      };
    } catch {
      // ignore notification errors
    }
  }
}
