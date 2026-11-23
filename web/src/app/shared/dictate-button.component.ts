import { ChangeDetectionStrategy, Component, DestroyRef, inject, input, output, signal } from '@angular/core';
import {
  SpeechRecognitionLike,
  joinDictation,
  speechErrorMessage,
  speechRecognitionCtor,
  transcriptOf,
} from '../features/chat/composer/speech-dictation';
import { IconComponent } from './icon.component';
import { TooltipDirective } from './tooltip.directive';

/**
 * Mic button that dictates into a text field with the composer's speech-to-text (Web
 * Speech API). `base` is the field's text when dictation starts; `text` emits the field's
 * new text as speech arrives. Hidden where the browser has no speech recognition.
 */
@Component({
  selector: 'ks-dictate-button',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  template: `
    @if (supported) {
      <button
        type="button"
        class="ks-icon-btn"
        data-testid="dictate-button"
        [class.ks-mic-recording]="dictating()"
        [attr.aria-pressed]="dictating()"
        [attr.aria-label]="dictating() ? 'Stop dictation' : 'Dictate'"
        [ksTooltip]="dictating() ? 'Stop dictation' : 'Dictate'"
        (click)="toggle()"
      >
        <app-icon name="mic" [size]="15" />
      </button>
      <span class="ks-sr-only" role="status" aria-live="polite">{{ dictating() ? 'Listening.' : error() }}</span>
    }
  `,
})
export class DictateButtonComponent {
  readonly base = input('');
  readonly text = output<string>();

  readonly supported = !!speechRecognitionCtor();
  readonly dictating = signal(false);
  readonly error = signal('');
  private rec: SpeechRecognitionLike | null = null;
  private startText = '';

  constructor() {
    inject(DestroyRef).onDestroy(() => this.rec?.abort());
  }

  toggle(): void {
    if (this.rec) {
      this.rec.stop();
      this.dictating.set(false);
      return;
    }
    const Ctor = speechRecognitionCtor();
    if (!Ctor) return;
    this.error.set('');
    this.startText = this.base();
    const rec = new Ctor();
    rec.continuous = true;
    rec.interimResults = true;
    rec.lang = typeof navigator !== 'undefined' ? navigator.language || 'en-US' : 'en-US';
    rec.onresult = (ev) => {
      if (this.rec !== rec) return;
      const { final, interim } = transcriptOf(ev);
      this.text.emit(joinDictation(this.startText, final + interim));
    };
    rec.onerror = (ev) => this.error.set(speechErrorMessage(ev.error));
    rec.onend = () => {
      if (this.rec === rec) {
        this.rec = null;
        this.dictating.set(false);
      }
    };
    this.rec = rec;
    this.dictating.set(true);
    try {
      rec.start();
    } catch {
      this.rec = null;
      this.dictating.set(false);
      this.error.set('Dictation could not start.');
    }
  }
}
