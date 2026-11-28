import { Injectable, computed, inject, signal } from '@angular/core';
import { ApiClientService } from '../services/api-client.service';

/** A login with its display name (SYR-98VFV). */
export interface Person {
  login: string;
  display_name?: string;
  /** The name the user set themselves ('' = detected). */
  override?: string;
}

/**
 * Display names for people: "Bryan Sant" instead of "bsant" wherever a person appears.
 * Agents ("agent/claude", "commander") have no entry and keep their labels.
 */
@Injectable({ providedIn: 'root' })
export class PeopleStore {
  private readonly api = inject(ApiClientService);
  readonly people = signal<Record<string, Person>>({});
  private loaded = false;

  readonly byLogin = computed(() => this.people());

  async load(): Promise<void> {
    if (this.loaded) return;
    this.loaded = true;
    try {
      const res = await this.api.getPeople();
      this.people.set(Object.fromEntries(res.people.map((p) => [p.login, p])));
    } catch {
      this.loaded = false;
    }
  }

  apply(p: Person): void {
    this.people.update((m) => ({ ...m, [p.login]: p }));
  }

  /** The display name for login, else the login itself. */
  nameOf(login: string | undefined | null): string {
    if (!login) return '';
    return this.people()[login]?.display_name || login;
  }
}

/** What a person shows as, and the tooltip (the login, only when it differs). */
export function personLabel(
  login: string,
  display: string | undefined,
): { text: string; tip: string } {
  const name = (display ?? '').trim();
  if (!name || name === login) return { text: login, tip: '' };
  return { text: name, tip: login };
}
