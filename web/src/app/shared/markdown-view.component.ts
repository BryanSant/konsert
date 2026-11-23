import { NgTemplateOutlet } from '@angular/common';
import { Component, computed, input, signal } from '@angular/core';
import { IconComponent } from './icon.component';

export interface InlineToken {
  kind: 'text' | 'bold' | 'italic' | 'code' | 'link';
  text: string;
  href?: string;
}

export interface MarkdownBlock {
  id: number;
  kind: 'h1' | 'h2' | 'h3' | 'code' | 'ul' | 'ol' | 'hr' | 'p';
  lang?: string;
  raw?: string;
  tokens?: InlineToken[];
  items?: InlineToken[][];
}

@Component({
  selector: 'app-markdown-view',
  standalone: true,
  imports: [IconComponent, NgTemplateOutlet],
  template: `
    <!-- Inline tokens (code / bold / italic / link / text), shared by every block kind. -->
    <ng-template #inline let-tokens let-codeSize="codeSize">
      @for (t of tokens; track $index) {
        @switch (t.kind) {
          @case ('code') { <code class="font-mono-ide rounded bg-[#23252c] px-1.5 py-0.5 text-[#d4d4d8]" [class]="codeSize">{{ t.text }}</code> }
          @case ('bold') { <strong class="font-semibold text-[#f4f4f5]">{{ t.text }}</strong> }
          @case ('italic') { <em class="italic">{{ t.text }}</em> }
          @case ('link') { <a [href]="t.href" target="_blank" rel="noopener noreferrer" class="text-[#7aa8e8] underline hover:text-[#9ec2f5]">{{ t.text }}</a> }
          @default { {{ t.text }} }
        }
      }
    </ng-template>

    <div class="space-y-2.5 text-[13px] text-[#e4e4e7] leading-relaxed break-words">
      @for (block of blocks(); track block.id) {
        @switch (block.kind) {
          @case ('h1') {
            <h1 class="text-[16px] font-semibold text-[#f4f4f5] pt-1"><ng-container *ngTemplateOutlet="inline; context: { $implicit: block.tokens ?? [], codeSize: 'text-[12px]' }" /></h1>
          }
          @case ('h2') {
            <h2 class="text-[14.5px] font-semibold text-[#f4f4f5] pt-1"><ng-container *ngTemplateOutlet="inline; context: { $implicit: block.tokens ?? [], codeSize: 'text-[12px]' }" /></h2>
          }
          @case ('h3') {
            <h3 class="text-[13.5px] font-semibold text-[#ececed] pt-0.5"><ng-container *ngTemplateOutlet="inline; context: { $implicit: block.tokens ?? [], codeSize: 'text-[12px]' }" /></h3>
          }
          @case ('hr') {
            <hr class="border-[#26282f] my-1" />
          }
          @case ('code') {
            <div class="rounded-lg border border-[#282a32] bg-[#101114] overflow-hidden">
              <div class="flex items-center justify-between px-3 py-1.5 border-b border-[#22242b] bg-[#15161a] text-[10.5px] text-[#8a8c94]">
                <span class="font-mono-ide">{{ block.lang || 'code' }}</span>
                <button
                  type="button"
                  (click)="copyCode(block.id, block.raw || '')"
                  [attr.aria-label]="'Copy ' + (block.lang || 'code') + ' block'"
                  class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:bg-[#24262e] text-[#9a9ca4] hover:text-[#ececed] transition cursor-pointer"
                >
                  <app-icon name="copy" [size]="11" />
                  <span>{{ copiedBlockId() === block.id ? 'Copied' : 'Copy' }}</span>
                </button>
              </div>
              <pre class="overflow-x-auto p-3 font-mono-ide text-[11.5px] text-[#d4d4d8] leading-relaxed"><code>{{ block.raw }}</code></pre>
            </div>
          }
          @case ('ul') {
            <ul class="list-disc pl-5 space-y-1">
              @for (item of block.items ?? []; track $index) {
                <li><ng-container *ngTemplateOutlet="inline; context: { $implicit: item, codeSize: 'text-[11.5px]' }" /></li>
              }
            </ul>
          }
          @case ('ol') {
            <ol class="list-decimal pl-5 space-y-1">
              @for (item of block.items ?? []; track $index) {
                <li><ng-container *ngTemplateOutlet="inline; context: { $implicit: item, codeSize: 'text-[11.5px]' }" /></li>
              }
            </ol>
          }
          @default {
            <p class="whitespace-pre-wrap"><ng-container *ngTemplateOutlet="inline; context: { $implicit: block.tokens ?? [], codeSize: 'text-[11.5px]' }" /></p>
          }
        }
      }
    </div>
  `,
})
export class MarkdownViewComponent {
  readonly content = input<string>('');
  readonly copiedBlockId = signal<number | null>(null);

  readonly blocks = computed<MarkdownBlock[]>(() => parseMarkdown(this.content() || ''));

  async copyCode(blockId: number, raw: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(raw);
      this.copiedBlockId.set(blockId);
      setTimeout(() => {
        if (this.copiedBlockId() === blockId) {
          this.copiedBlockId.set(null);
        }
      }, 1600);
    } catch {
      // ignore
    }
  }
}

export function parseMarkdown(src: string): MarkdownBlock[] {
  const lines = src.replace(/\r\n/g, '\n').split('\n');
  const blocks: MarkdownBlock[] = [];
  let nextId = 1;
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Blank line
    if (!line.trim()) {
      i++;
      continue;
    }

    // Fenced code block
    const fenceMatch = line.match(/^```([\w.+-]*)\s*$/);
    if (fenceMatch) {
      const lang = fenceMatch[1] || '';
      const codeLines: string[] = [];
      i++;
      while (i < lines.length && !lines[i].startsWith('```')) {
        codeLines.push(lines[i]);
        i++;
      }
      if (i < lines.length && lines[i].startsWith('```')) {
        i++;
      }
      blocks.push({ id: nextId++, kind: 'code', lang, raw: codeLines.join('\n') });
      continue;
    }

    // Horizontal rule
    if (/^(-{3,}|\*{3,}|_{3,})\s*$/.test(line.trim())) {
      blocks.push({ id: nextId++, kind: 'hr' });
      i++;
      continue;
    }

    // Headings
    const hMatch = line.match(/^(#{1,3})\s+(.+)$/);
    if (hMatch) {
      const level = hMatch[1].length;
      blocks.push({
        id: nextId++,
        kind: level === 1 ? 'h1' : level === 2 ? 'h2' : 'h3',
        tokens: tokenizeInline(hMatch[2]),
      });
      i++;
      continue;
    }

    // Unordered list
    if (/^\s*[-*+]\s+/.test(line)) {
      const items: InlineToken[][] = [];
      while (i < lines.length && /^\s*[-*+]\s+/.test(lines[i])) {
        items.push(tokenizeInline(lines[i].replace(/^\s*[-*+]\s+/, '')));
        i++;
      }
      blocks.push({ id: nextId++, kind: 'ul', items });
      continue;
    }

    // Ordered list
    if (/^\s*\d+\.\s+/.test(line)) {
      const items: InlineToken[][] = [];
      while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) {
        items.push(tokenizeInline(lines[i].replace(/^\s*\d+\.\s+/, '')));
        i++;
      }
      blocks.push({ id: nextId++, kind: 'ol', items });
      continue;
    }

    // Paragraph (gather consecutive non-special lines)
    const paraLines: string[] = [line];
    i++;
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !lines[i].startsWith('```') &&
      !/^#{1,3}\s+/.test(lines[i]) &&
      !/^\s*[-*+]\s+/.test(lines[i]) &&
      !/^\s*\d+\.\s+/.test(lines[i]) &&
      !/^(-{3,}|\*{3,}|_{3,})\s*$/.test(lines[i].trim())
    ) {
      paraLines.push(lines[i]);
      i++;
    }
    blocks.push({ id: nextId++, kind: 'p', tokens: tokenizeInline(paraLines.join('\n')) });
  }

  return blocks;
}

export function tokenizeInline(text: string): InlineToken[] {
  const tokens: InlineToken[] = [];
  const regex = /(`[^`]+`|\*\*[^*]+\*\*|\*[^*\n]+\*|\[[^\]]+\]\(https?:\/\/[^\s)]+\))/g;
  let lastIndex = 0;
  let match: RegExpExecArray | null;

  while ((match = regex.exec(text)) !== null) {
    if (match.index > lastIndex) {
      tokens.push({ kind: 'text', text: text.slice(lastIndex, match.index) });
    }
    const raw = match[0];
    if (raw.startsWith('`') && raw.endsWith('`')) {
      tokens.push({ kind: 'code', text: raw.slice(1, -1) });
    } else if (raw.startsWith('**') && raw.endsWith('**')) {
      tokens.push({ kind: 'bold', text: raw.slice(2, -2) });
    } else if (raw.startsWith('*') && raw.endsWith('*')) {
      tokens.push({ kind: 'italic', text: raw.slice(1, -1) });
    } else if (raw.startsWith('[')) {
      const m = raw.match(/^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/);
      if (m) {
        tokens.push({ kind: 'link', text: m[1], href: m[2] });
      } else {
        tokens.push({ kind: 'text', text: raw });
      }
    }
    lastIndex = match.index + raw.length;
  }

  if (lastIndex < text.length) {
    tokens.push({ kind: 'text', text: text.slice(lastIndex) });
  }
  return tokens;
}
