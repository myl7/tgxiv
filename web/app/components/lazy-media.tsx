"use client";

import { useRef, useState, useEffect, type ReactNode } from "react";

interface LazyMediaProps {
    src: string;
    /** If true, stop observing after first intersection (images). If false, remove src when leaving viewport (video/audio). */
    once?: boolean;
    rootMargin?: string;
    className?: string;
    children: (src: string | undefined) => ReactNode;
}

export function LazyMedia({
    src,
    once = true,
    rootMargin = "200px",
    className,
    children,
}: LazyMediaProps) {
    const ref = useRef<HTMLDivElement>(null);
    const [activeSrc, setActiveSrc] = useState<string | undefined>(undefined);

    useEffect(() => {
        const el = ref.current;
        if (!el) return;

        const observer = new IntersectionObserver(
            ([entry]) => {
                if (entry.isIntersecting) {
                    setActiveSrc(src);
                    if (once) observer.disconnect();
                } else if (!once) {
                    setActiveSrc(undefined);
                }
            },
            { rootMargin, threshold: 0 },
        );

        observer.observe(el);
        return () => observer.disconnect();
    }, [src, once, rootMargin]);

    return (
        <div ref={ref} className={className}>
            {children(activeSrc)}
        </div>
    );
}
