// transition-manager.js

export class TransitionManager {
	constructor() {
		this.app = document.getElementById("app");
		this.customTransitions = new Map();
		this.transitionContext = {};
	}

	init() {
		this.setupEventListeners();
	}

	setupEventListeners() {
		document.body.addEventListener("click", this.handleClick.bind(this));
		window.addEventListener("popstate", this.handlePopState.bind(this));
	}

	handleClick(e) {
		const link = e.target.closest("a");
		if (link && link.origin === window.location.origin) {
			e.preventDefault();
			// Store information about the clicked element
			this.transitionContext = {
				clickedElement: link,
				clickedRect: link.getBoundingClientRect(),
				targetPath: new URL(link.href).pathname,
			};
			this.navigateTo(link.href);
		}
	}

	async loadContent(path) {
		try {
			const response = await fetch(`/__spa__${path}`);
			if (!response.ok) throw new Error("Page not found");

			const data = await response.json();

			if (document.startViewTransition) {
				await document.startViewTransition(() => {
					this.updateDOM(data);
					this.runCustomTransitions(path);
				}).finished;
			} else {
				this.updateDOM(data);
				this.runCustomTransitions(path);
			}
		} catch (error) {
			console.error("Error loading content:", error);
			this.app.innerHTML = "<h1>404 - Page Not Found</h1>";
		} finally {
			// Clear the context after the transition
			this.transitionContext = {};
		}
	}

	runCustomTransitions(path) {
		const transitionFunc = this.customTransitions.get(path);
		if (transitionFunc) {
			transitionFunc(this.transitionContext);
		}
	}

	handlePopState() {
		this.loadContent(window.location.pathname);
	}

	async navigateTo(url) {
		const pathname = new URL(url).pathname;
		if (document.startViewTransition) {
			await document.startViewTransition(() => {
				window.history.pushState({}, "", pathname);
				this.loadContent(pathname);
			}).finished;
		} else {
			window.history.pushState({}, "", pathname);
			await this.loadContent(pathname);
		}
	}

	updateDOM(data) {
		this.app.innerHTML = data.Content;

		// Apply styles
		const styleElement =
			document.getElementById("dynamic-style") ||
			document.createElement("style");
		styleElement.id = "dynamic-style";
		styleElement.textContent = data.Style;
		if (!styleElement.parentNode) {
			document.head.appendChild(styleElement);
		}

		// Execute script
		const scriptElement = document.createElement("script");
		scriptElement.textContent = data.Script;
		document.body.appendChild(scriptElement);
	}

	registerCustomTransition(path, transitionFunc) {
		this.customTransitions.set(path, transitionFunc);
	}
}
