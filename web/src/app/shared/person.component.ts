import { ChangeDetectionStrategy, Component, OnInit, computed, inject, input } from '@angular/core';
import { PeopleStore, personLabel } from '../core/state/people.store';
import { TooltipDirective } from './tooltip.directive';

/**
 * A person, by display name (SYR-98VFV): "Bryan Sant", with the login ("bsant") on hover.
 * No tooltip when there is nothing more to say. Used wherever people appear.
 */
@Component({
  selector: 'sy-person',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [TooltipDirective],
  template: `@if (label().tip) {
      <span data-testid="person" [attr.data-login]="login()" [syTooltip]="label().tip">{{
        label().text
      }}</span>
    } @else {
      <span data-testid="person" [attr.data-login]="login()">{{ label().text }}</span>
    }`,
})
export class PersonComponent implements OnInit {
  private readonly people = inject(PeopleStore);
  readonly login = input('');
  readonly label = computed(() =>
    personLabel(this.login(), this.people.byLogin()[this.login()]?.display_name),
  );

  ngOnInit(): void {
    void this.people.load();
  }
}
