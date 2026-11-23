/**
 * Speech-to-text for the composer through the browser's Web Speech API
 * (SpeechRecognition / webkitSpeechRecognition). Chrome, Edge and Safari support it over
 * HTTPS (Tailscale Serve) or localhost; elsewhere the mic button stays hidden.
 */

/** The parts of the Web Speech API the composer uses (not in TypeScript's DOM lib). */
export interface SpeechRecognitionLike {
  continuous: boolean;
  interimResults: boolean;
  lang: string;
  onresult: ((ev: SpeechResultEventLike) => void) | null;
  onerror: ((ev: { error: string; message?: string }) => void) | null;
  onend: (() => void) | null;
  onstart?: (() => void) | null;
  start(): void;
  stop(): void;
  abort(): void;
}

export interface SpeechResultEventLike {
  resultIndex: number;
  results: ArrayLike<ArrayLike<{ transcript: string }> & { isFinal: boolean }>;
}

export type SpeechRecognitionCtor = new () => SpeechRecognitionLike;

/** The browser's SpeechRecognition constructor, or null when unsupported. */
export function speechRecognitionCtor(win: unknown = typeof window === 'undefined' ? undefined : window): SpeechRecognitionCtor | null {
  const w = win as { SpeechRecognition?: SpeechRecognitionCtor; webkitSpeechRecognition?: SpeechRecognitionCtor } | undefined;
  return w?.SpeechRecognition ?? w?.webkitSpeechRecognition ?? null;
}

/** Final and interim text of a (continuous) recognition session so far. */
export function transcriptOf(ev: SpeechResultEventLike): { final: string; interim: string } {
  let final = '';
  let interim = '';
  for (let i = 0; i < ev.results.length; i++) {
    const r = ev.results[i];
    const text = r[0]?.transcript ?? '';
    if (r.isFinal) final += text;
    else interim += text;
  }
  return { final, interim };
}

/** Joins the text that was in the prompt before dictation with the dictated text. */
export function joinDictation(base: string, spoken: string): string {
  const s = spoken.replace(/^\s+/, '');
  if (!s) return base;
  if (!base) return s;
  return /\s$/.test(base) ? base + s : `${base} ${s}`;
}

/** A user-facing message for a SpeechRecognition error code ('' = nothing to show). */
export function speechErrorMessage(code: string): string {
  switch (code) {
    case 'not-allowed':
    case 'service-not-allowed':
      return "Microphone access is blocked. Allow it in the browser's site settings to dictate.";
    case 'audio-capture':
      return 'No microphone was found.';
    case 'no-speech':
      return 'No speech was heard. Try again.';
    case 'network':
      return 'Dictation needs a network connection in this browser.';
    case 'language-not-supported':
      return "Dictation doesn't support this language.";
    case 'aborted':
      return '';
    default:
      return 'Dictation stopped unexpectedly.';
  }
}
