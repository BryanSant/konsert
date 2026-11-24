import { Component, input } from '@angular/core';

@Component({
  selector: 'app-icon',
  standalone: true,
  // Icons are decorative: the surrounding control carries the accessible name.
  host: { 'aria-hidden': 'true' },
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
      @case ('codex') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#74d6a8]">
          <path d="M8 2.2 13 5.1v5.8L8 13.8 3 10.9V5.1L8 2.2Z" stroke="currentColor" stroke-width="1.35" stroke-linejoin="round"/>
          <circle cx="8" cy="8" r="1.8" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('copilot') {
        <!-- Generic visor/goggles mark in GitHub purple (not the Copilot logo). -->
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#a371f7]">
          <path d="M2.2 9.2V7.4C2.2 4.6 4.8 2.8 8 2.8s5.8 1.8 5.8 4.6v1.8c0 2.4-2.6 4-5.8 4s-5.8-1.6-5.8-4Z" stroke="currentColor" stroke-width="1.35" stroke-linejoin="round"/>
          <rect x="4.2" y="6.2" width="7.6" height="3.4" rx="1.7" stroke="currentColor" stroke-width="1.25"/>
          <path d="M6.3 7.4v1M9.7 7.4v1" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/>
        </svg>
      }
      @case ('cursor') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0 text-[#a5b4fc]">
          <path d="M3.2 2.8 12.8 7.5 8.2 9l-1.5 4.2-3.5-10.4Z" stroke="currentColor" stroke-width="1.35" stroke-linejoin="round"/>
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
      @case ('phone') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="4.5" y="1.8" width="7" height="12.4" rx="1.6" stroke="currentColor" stroke-width="1.3"/>
          <path d="M7.2 11.9h1.6" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/>
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
      @case ('bookmark') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" [attr.fill]="filled() ? 'currentColor' : 'none'" class="shrink-0">
          <path d="M4.5 2.5h7v11L8 11l-3.5 2.5v-11Z" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>
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
      @case ('folder') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M2.2 4.3A1.3 1.3 0 0 1 3.5 3h2.8l1.4 1.5h4.8a1.3 1.3 0 0 1 1.3 1.3v5.9a1.3 1.3 0 0 1-1.3 1.3h-9a1.3 1.3 0 0 1-1.3-1.3V4.3Z" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>
        </svg>
      }
      @case ('check-circle') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <circle cx="8" cy="8" r="5.6" stroke="currentColor" stroke-width="1.3"/>
          <path d="m5.6 8.2 1.7 1.7 3.3-3.5" stroke="currentColor" stroke-width="1.35" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('trash') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M3 4.5h10M6 4.5V3.3c0-.44.36-.8.8-.8h2.4c.44 0 .8.36.8.8v1.2M4.5 4.5l.6 8.2c.04.45.42.8.87.8h4.06c.45 0 .83-.35.87-.8l.6-8.2" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('paperclip') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M11.6 5.2 6.4 10.4a1.6 1.6 0 0 1-2.26-2.26l5.8-5.8a2.7 2.7 0 1 1 3.82 3.82l-6 6a3.8 3.8 0 0 1-5.38-5.38l5.1-5.1" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('edit') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M11.2 2.8a1.4 1.4 0 0 1 2 2L5.6 12.4 2.5 13.5l1.1-3.1 7.6-7.6Z" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>
        </svg>
      }
      @case ('split') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2" y="2.5" width="12" height="11" rx="1.8" stroke="currentColor" stroke-width="1.3"/>
          <path d="M8 2.5v11" stroke="currentColor" stroke-width="1.3"/>
        </svg>
      }
      @case ('workspace') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M2.5 5.2 8 2.2l5.5 3-5.5 3-5.5-3ZM2.5 8.3 8 11.3l5.5-3M2.5 11.1 8 14.1l5.5-3" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('project') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="2.2" y="2.5" width="11.6" height="11" rx="2" stroke="currentColor" stroke-width="1.3"/>
          <path d="M2.2 6h11.6M6.5 6v7.5" stroke="currentColor" stroke-width="1.25"/>
        </svg>
      }
      @case ('repo') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M3.5 2.5h8.2a1.3 1.3 0 0 1 1.3 1.3v8.4a1.3 1.3 0 0 1-1.3 1.3H3.5V2.5Z" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>
          <path d="M6 2.5v11" stroke="currentColor" stroke-width="1.2"/>
        </svg>
      }
      @case ('bell') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M4.2 10.8V7a3.8 3.8 0 0 1 7.6 0v3.8l1.2 1.4H3l1.2-1.4ZM6.6 12.4a1.4 1.4 0 0 0 2.8 0" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      }
      @case ('info') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <circle cx="8" cy="8" r="5.6" stroke="currentColor" stroke-width="1.3"/>
          <path d="M8 7.2v3.6M8 5.2h.01" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>
        </svg>
      }
      @case ('hand') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M5.5 8V3.6a1 1 0 0 1 2 0V7.5M7.5 7V2.8a1 1 0 0 1 2 0V7M9.5 7.2V3.8a1 1 0 0 1 2 0v5.4c0 2.7-1.7 4.6-4.2 4.6-1.6 0-2.6-.7-3.4-1.9L2.6 9.6a1 1 0 0 1 1.6-1.2l1.3 1.4"/>
        </svg>
      }
      @case ('shield-check') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M8 1.8 13 3.6v4c0 3.1-2.1 5.4-5 6.6-2.9-1.2-5-3.5-5-6.6v-4L8 1.8Z"/><path d="m5.7 8 1.6 1.6 3-3.1"/>
        </svg>
      }
      @case ('shield-alert') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M8 1.8 13 3.6v4c0 3.1-2.1 5.4-5 6.6-2.9-1.2-5-3.5-5-6.6v-4L8 1.8Z"/><path d="M8 5.2v3.2M8 10.6v.1"/>
        </svg>
      }
      @case ('eye') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M1.6 8s2.3-4.3 6.4-4.3S14.4 8 14.4 8 12.1 12.3 8 12.3 1.6 8 1.6 8Z"/><circle cx="8" cy="8" r="1.9"/>
        </svg>
      }
      @case ('mic') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <rect x="5.8" y="1.8" width="4.4" height="7.6" rx="2.2"/><path d="M3.6 7.6a4.4 4.4 0 0 0 8.8 0M8 12v2.2"/>
        </svg>
      }
      @case ('check') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="m3.4 8.4 3 3 6.2-6.6"/>
        </svg>
      }
      @case ('x') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="m4.2 4.2 7.6 7.6M11.8 4.2l-7.6 7.6"/>
        </svg>
      }
      @case ('x-circle') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <circle cx="8" cy="8" r="6"/><path d="m5.9 5.9 4.2 4.2M10.1 5.9l-4.2 4.2"/>
        </svg>
      }
      @case ('pause') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <rect x="4.2" y="3.6" width="2.4" height="8.8" rx="0.8" fill="currentColor"/>
          <rect x="9.4" y="3.6" width="2.4" height="8.8" rx="0.8" fill="currentColor"/>
        </svg>
      }
      @case ('play') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" class="shrink-0">
          <path d="M5 3.6v8.8a.6.6 0 0 0 .9.5l6.9-4.4a.6.6 0 0 0 0-1L5.9 3.1a.6.6 0 0 0-.9.5Z" fill="currentColor"/>
        </svg>
      }
      @case ('ban') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" class="shrink-0">
          <circle cx="8" cy="8" r="5.6"/>
          <path d="M4.1 4.1l7.8 7.8"/>
        </svg>
      }
      @case ('drain') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M3 4.5h10M5 8h6M7 11.5h2"/>
        </svg>
      }
      @case ('stop') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <rect x="4.2" y="4.2" width="7.6" height="7.6" rx="1.6" fill="currentColor" stroke="none"/>
        </svg>
      }
      @case ('brain') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M6 2.6a2 2 0 0 0-2 2v.2A2.2 2.2 0 0 0 2.8 9a2.2 2.2 0 0 0 1.6 3.4A2 2 0 0 0 8 13V3.6a1.5 1.5 0 0 0-2-1Z"/><path d="M10 2.6a2 2 0 0 1 2 2v.2A2.2 2.2 0 0 1 13.2 9a2.2 2.2 0 0 1-1.6 3.4A2 2 0 0 1 8 13"/>
        </svg>
      }
      @case ('file') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M4 1.8h5l3 3v9.4H4V1.8Z"/><path d="M9 1.8v3h3"/>
        </svg>
      }
      @case ('file-edit') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M8.6 14.2H4V1.8h5l3 3v3"/><path d="m9.6 13.6.3-1.7 3.3-3.3 1.4 1.4-3.3 3.3-1.7.3Z"/>
        </svg>
      }
      @case ('globe') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <circle cx="8" cy="8" r="6"/><path d="M2 8h12M8 2c1.8 2 2.6 4 2.6 6S9.8 12 8 14C6.2 12 5.4 10 5.4 8S6.2 4 8 2Z"/>
        </svg>
      }
      @case ('wrench') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M10.2 2.4a3.2 3.2 0 0 0-3.9 4.3L2.4 10.6a1.4 1.4 0 0 0 2 2l3.9-3.9a3.2 3.2 0 0 0 4.3-3.9l-1.9 1.9-1.6-.4-.4-1.6 1.5-2.3Z"/>
        </svg>
      }
      @case ('list-todo') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="m2.6 4 1.2 1.2L6 3M2.6 9.4l1.2 1.2L6 8.4M8.4 4.2h5M8.4 9.6h5"/>
        </svg>
      }
      @case ('question') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <circle cx="8" cy="8" r="6"/><path d="M6.3 6.2a1.8 1.8 0 0 1 3.5.5c0 1.2-1.8 1.5-1.8 2.6M8 11.3v.1"/>
        </svg>
      }
      @case ('key') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <circle cx="5.4" cy="10.6" r="2.8"/><path d="m7.4 8.6 6-6M11.4 4.6l1.6 1.6M9.8 6.2l1.2 1.2"/>
        </svg>
      }
      @case ('alert') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M8 2.2 14.2 13H1.8L8 2.2Z"/><path d="M8 6.4v3M8 11.2v.1"/>
        </svg>
      }
      @case ('refresh') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M13.2 7.2A5.2 5.2 0 0 0 3.6 5M2.8 8.8A5.2 5.2 0 0 0 12.4 11"/><path d="M3.4 2.6V5.2H6M12.6 13.4v-2.6H10"/>
        </svg>
      }
      @case ('arrow-down') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M8 3v10M3.8 8.8 8 13l4.2-4.2"/>
        </svg>
      }
      @case ('sparkle') {
        <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" class="shrink-0">
          <path d="M8 2.2 9.3 6.7 13.8 8 9.3 9.3 8 13.8 6.7 9.3 2.2 8 6.7 6.7 8 2.2Z"/>
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
  /** Filled variant (icons that support it, e.g. bookmark when on). */
  readonly filled = input(false);
}
