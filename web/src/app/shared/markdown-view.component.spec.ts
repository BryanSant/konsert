import { TestBed } from '@angular/core/testing';
import { afterEach, describe, expect, it } from 'vitest';
import { MarkdownViewComponent, parseMarkdown, tokenizeInline } from './markdown-view.component';

const HOSTILE = [
  '[click](javascript:alert(1))',
  '[click](JaVaScRiPt:alert(1))',
  '[click]( javascript:alert(1))',
  '[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)',
  '[click](vbscript:msgbox(1))',
  '[click](//evil.example/x)',
  '[click](file:///etc/passwd)',
  '[x](https://ok.example)(javascript:alert(1))',
  '<a href="javascript:alert(1)">raw html</a>',
  '<img src=x onerror=alert(1)>',
  '[click](https://ok.example" onmouseover="alert(1))',
];

describe('markdown tokenizer', () => {
  afterEach(() => TestBed.resetTestingModule());

  it('only ever produces http(s) link hrefs', () => {
    for (const src of HOSTILE) {
      for (const tok of tokenizeInline(src)) {
        if (tok.kind === 'link') {
          expect(tok.href, src).toMatch(/^https?:\/\/[^\s"'<>]+$/);
        }
      }
    }
  });

  it('keeps javascript:/data: links as plain text', () => {
    const toks = tokenizeInline('[click](javascript:alert(1))');
    expect(toks.every((t) => t.kind === 'text')).toBe(true);
    expect(toks.map((t) => t.text).join('')).toBe('[click](javascript:alert(1))');
  });

  it('tokenizes ordinary inline markup', () => {
    expect(tokenizeInline('a `b` **c** *d* [e](https://x.example/p)')).toEqual([
      { kind: 'text', text: 'a ' },
      { kind: 'code', text: 'b' },
      { kind: 'text', text: ' ' },
      { kind: 'bold', text: 'c' },
      { kind: 'text', text: ' ' },
      { kind: 'italic', text: 'd' },
      { kind: 'text', text: ' ' },
      { kind: 'link', text: 'e', href: 'https://x.example/p' },
    ]);
  });

  it('parses block structure', () => {
    const kinds = parseMarkdown('# H\n\n1. one\n2. two\n\n---\n\npara\nline').map((b) => b.kind);
    expect(kinds).toEqual(['h1', 'ol', 'hr', 'p']);
  });

  it('renders hostile input with no javascript: hrefs and no injected elements', () => {
    const fixture = TestBed.createComponent(MarkdownViewComponent);
    fixture.componentRef.setInput(
      'content',
      HOSTILE.join('\n\n') + '\n\n- [ok](https://ok.example)',
    );
    fixture.detectChanges();
    const root = fixture.nativeElement as HTMLElement;
    const anchors = Array.from(root.querySelectorAll('a'));
    expect(anchors.length).toBeGreaterThan(0);
    for (const a of anchors) {
      expect(a.getAttribute('href') ?? '').toMatch(/^https?:\/\//);
      expect(a.getAttribute('rel')).toContain('noopener');
      expect(a.getAttributeNames().some((n) => n.startsWith('on'))).toBe(false);
    }
    expect(root.querySelector('img, script, iframe')).toBeNull();
    // Raw HTML in the source is shown as text, never parsed into attributes.
    expect(
      root.querySelector('[href^="javascript" i], [src], [onerror], [onmouseover]'),
    ).toBeNull();
    expect(root.textContent).toContain('<a href="javascript:alert(1)">raw html</a>');
  });
});
