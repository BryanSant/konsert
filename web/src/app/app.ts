import { Component, OnInit, inject } from '@angular/core';
import { AuthService } from './core/services/auth.service';
import { LoginViewComponent } from './features/auth/login-view.component';
import { AppShellComponent } from './layout/app-shell.component';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [LoginViewComponent, AppShellComponent],
  template: `
    @if (auth.loading()) {
      <div class="h-screen w-screen flex items-center justify-center bg-[#111214] text-[#8e9098] text-[13px] font-mono-ide">
        Connecting to Konsert...
      </div>
    } @else if (!auth.authenticated()) {
      <app-login-view />
    } @else {
      <app-shell />
    }
  `,
})
export class App implements OnInit {
  readonly auth = inject(AuthService);

  ngOnInit(): void {
    void this.auth.checkAuth();
  }
}
