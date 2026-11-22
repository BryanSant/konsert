import { Component, input } from '@angular/core';

@Component({
  selector: 'app-icon',
  standalone: true,
  template: `
    @switch (name()) {
      @case ('claude') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#d97757]">
          <path d="M8 1.5v13M2.4 4.75l11.2 6.5M13.6 4.75l-11.2 6.5" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/>
        </svg>
      }
      @case ('agy') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#60a5fa]">
          <path d="M8 2.2 13.4 13.4 8 10.6 2.6 13.4 8 2.2Z" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/>
        </svg>
      }
      @case ('grok') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#e4e4e7]">
          <circle cx="8" cy="8" r="5.5" stroke="currentColor" stroke-width="1.4"/>
          <path d="M4.2 11.8 11.8 4.2" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
        </svg>
      }
      @case ('shell') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#9ca3af]">
          <path d="M3 4.5 6.8 8 3 11.5M8.2 11.5H13" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('mock') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#a78bfa]">
          <path d="M9.2 1.8 3.8 9h4l-.8 5.2L12.4 7h-4l.8-5.2Z" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/>
        </svg>
      }
      @case ('orchestrator') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M8 1.8 9.6 6.4 14.2 8 9.6 9.6 8 14.2 6.4 9.6 1.8 8 6.4 6.4 8 1.8Z" stroke="currentColor" stroke-width="1.35" stroke-linejoin="round"/>
        </svg>
      }
      @case ('sidebar-left') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2" y="2.5" width="12" height="11" rx="2" stroke="currentColor" stroke-width="1.3"/>
          <path d="M6 2.5v11" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('sidebar-right') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2" y="2.5" width="12" height="11" rx="2" stroke="currentColor" stroke-width="1.3"/>
          <path d="M10 2.5v11" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('search') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <circle cx="7.2" cy="7.2" r="4.5" stroke="currentColor" stroke-width="1.35"/>
          <path d="m10.7 10.7 2.8 2.8" stroke="currentColor" stroke-width="1.35" stroke-linecap="round"/>
        </svg>
      }
      @case ('plus') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M8 3.2v9.6M3.2 8h9.6" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
        </svg>
      }
      @case ('close') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="m4.2 4.2 7.6 7.6M11.8 4.2l-7.6 7.6" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>
        </svg>
      }
      @case ('monitor') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2" y="2.5" width="12" height="8.5" rx="1.5" stroke="currentColor" stroke-width="1.3"/>
          <path d="M5.5 13.5h5M8 11v2.5" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/>
        </svg>
      }
      @case ('server') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2.5" y="2.5" width="11" height="4.5" rx="1.2" stroke="currentColor" stroke-width="1.3"/>
          <rect x="2.5" y="9" width="11" height="4.5" rx="1.2" stroke="currentColor" stroke-width="1.3"/>
          <circle cx="5" cy="4.75" r=".7" fill="currentColor"/>
          <circle cx="5" cy="11.25" r=".7" fill="currentColor"/>
        </svg>
      }
      @case ('container') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M8 2 13.5 5v6L8 14 2.5 11V5L8 2Z" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>
          <path d="M2.5 5 8 8l5.5-3M8 8v6" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>
        </svg>
      }
      @case ('git-branch') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <circle cx="4.5" cy="3.5" r="1.5" stroke="currentColor" stroke-width="1.3"/>
          <circle cx="4.5" cy="12.5" r="1.5" stroke="currentColor" stroke-width="1.3"/>
          <circle cx="11.5" cy="5.5" r="1.5" stroke="currentColor" stroke-width="1.3"/>
          <path d="M4.5 5v6M11.5 7c0 2.5-3 2.5-7 2.5" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('agent-history') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2.5" y="2.5" width="4.5" height="4.5" rx="1" stroke="currentColor" stroke-width="1.3"/>
          <rect x="9" y="2.5" width="4.5" height="4.5" rx="1" stroke="currentColor" stroke-width="1.3"/>
          <rect x="2.5" y="9" width="4.5" height="4.5" rx="1" stroke="currentColor" stroke-width="1.3"/>
          <rect x="9" y="9" width="4.5" height="4.5" rx="1" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('checks') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M3 4.5h3.5M8.5 4.5H13M3 8h3.5M8.5 8H13M3 11.5h3.5M8.5 11.5H13" stroke="currentColor" stroke-width="1.35" stroke-linecap="round"/>
        </svg>
      }
      @case ('lock') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="3.5" y="7" width="9" height="6.5" rx="1.5" stroke="currentColor" stroke-width="1.3"/>
          <path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('unlock') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="3.5" y="7" width="9" height="6.5" rx="1.5" stroke="currentColor" stroke-width="1.3"/>
          <path d="M5.5 7V5a2.5 2.5 0 0 1 4.7-1.2" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/>
        </svg>
      }
      @case ('shield') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M8 2 13 4v3.8c0 3.1-2.1 5.3-5 6.2-2.9-.9-5-3.1-5-6.2V4l5-2Z" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>
        </svg>
      }
      @case ('coffee') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M3 6h7.5v4.2a3.3 3.3 0 0 1-3.3 3.3H6.3A3.3 3.3 0 0 1 3 10.2V6Z" stroke="currentColor" stroke-width="1.3"/>
          <path d="M10.5 7h1.2a1.8 1.8 0 0 1 0 3.6h-1.2M5 3.5v1M7.5 3v1.5" stroke="currentColor" stroke-width="1.25" stroke-linecap="round"/>
        </svg>
      }
      @case ('activity') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M2 8.5h2.5l1.8-4.5 2.4 8 1.8-3.5H14" stroke="currentColor" stroke-width="1.35" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('ports') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M5.5 3v10M5.5 3 3.3 5.2M5.5 3l2.2 2.2M10.5 13V3M10.5 13l-2.2-2.2M10.5 13l2.2-2.2" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('scratch') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2.2" y="2.8" width="11.6" height="10.4" rx="2" stroke="currentColor" stroke-width="1.3"/>
          <path d="M2.2 6.5h11.6" stroke="currentColor" stroke-width="1.3"/>
          <path d="M5 9.5 6.8 11 5 12.2" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('send-up') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M8 12.5V3.5M4.2 7.2 8 3.4l3.8 3.8" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('settings') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <circle cx="8" cy="8" r="2.2" stroke="currentColor" stroke-width="1.3"/>
          <path d="M8 2v1.4M8 12.6V14M2 8h1.4M12.6 8H14M3.8 3.8l1 1M11.2 11.2l1 1M12.2 3.8l-1 1M4.8 11.2l-1 1" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/>
        </svg>
      }
      @case ('copy') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="5.5" y="5.5" width="8" height="8" rx="1.4" stroke="currentColor" stroke-width="1.3"/>
          <path d="M10.5 5.5V3.8A1.3 1.3 0 0 0 9.2 2.5H3.8A1.3 1.3 0 0 0 2.5 3.8v5.4a1.3 1.3 0 0 0 1.3 1.3h1.7" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('chevron-down') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="m4.5 6.2 3.5 3.6 3.5-3.6" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('chevron-right') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="m6.2 4.5 3.6 3.5-3.6 3.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('clock') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <circle cx="8" cy="8" r="5.5" stroke="currentColor" stroke-width="1.3"/>
          <path d="M8 5v3.2l2.1 1.4" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/>
        </svg>
      }
      @case ('tasks') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M5.5 4.2h7.5M5.5 8h7.5M5.5 11.8h7.5M2.8 4.2h.5M2.8 8h.5M2.8 11.8h.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>
        </svg>
      }
      @default {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <circle cx="8" cy="8" r="4" stroke="currentColor" stroke-width="1.4"/>
        </svg>
      }
    }
  `,
})
export class IconComponent {
  readonly name = input.required<string>();
  readonly size = input<number>(14);
}
