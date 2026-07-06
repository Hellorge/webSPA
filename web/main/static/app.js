// app.js

import { TransitionManager } from './transition-manager.js';

if (window.isSPAMode) {
    const transitionManager = new TransitionManager();
    transitionManager.init();

} else {
    console.log("Gogogo V3: Extreme Static Mode Active");
    
    // Auto-Hydration Intelligence
    const hydrate = async () => {
        const units = document.querySelectorAll('[data-live-query]');
        if (units.length === 0) return;

        console.log("Gogogo Hydrator: Commencing Live Sync...");
        
        for (const unit of units) {
            const query = unit.dataset.liveQuery;
            const page = unit.dataset.livePage || "home";
            
            try {
                const res = await fetch(`/api/live?page=${encodeURIComponent(page)}&q=${encodeURIComponent(query)}`);
                const data = await res.json();
                
                // If it's a list, we re-render specifically for the news intelligence pattern
                if (Array.isArray(data) && unit.classList.contains('news-grid')) {
                    unit.innerHTML = data.map(item => `
                        <article class="news-item animate-fresh">
                            <h3>${item.title}</h3>
                            <p>${item.summary}</p>
                            <time>${item.date}</time>
                        </article>
                    `).join('');
                }
            } catch (e) {
                console.error("Hydration Failure:", e);
            }
        }
    };

    window.addEventListener('DOMContentLoaded', hydrate);
}