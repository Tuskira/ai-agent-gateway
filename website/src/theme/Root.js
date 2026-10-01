import React, { useEffect, useRef } from 'react';
import { useLocation } from '@docusaurus/router';

export default function Root({ children }) {
  const { pathname } = useLocation();
  const ref = useRef(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const mainWrapper = el.querySelector('.main-wrapper');
    if (!mainWrapper) return;
    mainWrapper.style.animation = 'none';
    void mainWrapper.offsetHeight; // force reflow to restart animation
    mainWrapper.style.animation = '';
  }, [pathname]);

  return <div ref={ref}>{children}</div>;
}
