import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { App } from './app';

describe('App', () => {
  it('should create the root component', () => {
    TestBed.configureTestingModule({
      imports: [App],
    });
    const fixture = TestBed.createComponent(App);
    expect(fixture.componentInstance).toBeTruthy();
  });
});
