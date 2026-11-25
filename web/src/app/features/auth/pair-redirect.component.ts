import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { Router } from '@angular/router';

/**
 * The /pair/{code} route for a browser that is already signed in: there is nothing to
 * pair, so open the app. (Signed out, the app shows the pairing page instead and this
 * route keeps the code in the address bar.)
 */
@Component({
  selector: 'sy-pair-redirect',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: ``,
})
export class PairRedirectComponent {
  constructor() {
    const router = inject(Router);
    queueMicrotask(() => void router.navigateByUrl('/', { replaceUrl: true }));
  }
}
