/**
 * The james widget. The host page adds a <james-widget> element carrying a
 * token and optional settings. The element shows a button, opens the agent
 * popup and answers the browser tools the popup asks for. Each of those tools
 * is a module of its own under static/tools, loaded when it is first called.
 */

/**
 * What a tool module gets besides its input.
 * @typedef {Object} ToolContext
 * @property {function(string): string} t the interface text of a key
 * @property {function(string): Promise<boolean>} confirm asks the user a yes or
 *     no question in the popup, so it has to be awaited
 * @property {HTMLElement} element the widget element the call came through
 */

/**
 * Names of the browser tools the server offers. The server writes them here
 * when it delivers this script, so an unconfigured tool is never loaded.
 */
const toolNames = "__JAMES_TOOLS__".split(",").filter((name) => /^[a-z][a-z0-9_]*$/.test(name));

/** The origin the widget was loaded from, the only one it talks to. */
const serverOrigin = new URL(import.meta.url).origin;

/** The address of the popup page. */
const popupURL = new URL("./", import.meta.url).href;

/** The icon on the button, unless the element names another one. */
const iconURL = new URL("static/icon.svg", import.meta.url).href;

/** The styles of the button, shared by every widget. */
const sheet = new CSSStyleSheet();
sheet.replaceSync(`
	:host {
		position: fixed;
		right: 20px;
		bottom: 20px;
		z-index: 2147483000;
	}
	button {
		width: 56px;
		height: 56px;
		padding: 0;
		border: 0;
		border-radius: 50%;
		cursor: pointer;
		background: #2a78d6;
		box-shadow: 0 2px 10px rgba(0, 0, 0, .3);
		display: flex;
		align-items: center;
		justify-content: center;
	}
	button:hover {
		background: #256abf;
	}
	img {
		width: 28px;
		height: 28px;
		display: block;
	}
`);

/**
 * Reduce a language tag to one of the languages the widget knows.
 * @param {string} tag language tag such as "de-AT"
 * @returns {string} "de" or "en"
 */
function normalizeLang(tag) {
	return String(tag).slice(0, 2).toLowerCase() === "de" ? "de" : "en";
}

/**
 * Read an attribute holding a JSON object.
 * @param {?string} raw the attribute value
 * @returns {*} the parsed value, or an empty object when it is missing or broken
 */
function parseJSON(raw) {
	if (!raw) return {};
	try {
		return JSON.parse(raw);
	} catch {
		return {};
	}
}

/**
 * Report whether a window was just created and still shows nothing. A window
 * that shows the popup page is on another origin, which the check cannot read,
 * so it counts as in use.
 * @param {Window} win the window to check
 * @returns {boolean} true when the window is new
 */
function isBlank(win) {
	try {
		return win.location.href === "about:blank";
	} catch {
		return false;
	}
}

/** The interface texts of every language that was asked for, by language. */
const translations = new Map();

/**
 * Load the interface texts of one language. They come from the same files the
 * popup uses.
 * @param {string} lang the language code
 * @returns {Promise<Object>} the texts, empty when they cannot be read
 */
function loadTexts(lang) {
	if (!translations.has(lang)) {
		const url = new URL(`static/i18n/${lang}.json`, import.meta.url);
		translations.set(lang, fetch(url)
			.then((response) => (response.ok ? response.json() : {}))
			.catch(() => ({})));
	}
	return translations.get(lang);
}

/** The tool modules that were asked for, by name. */
const toolModules = new Map();

/**
 * Load one tool module. Every module exports the function that runs the tool
 * as its default export.
 * @param {string} name the tool name
 * @returns {Promise<function(Object, ToolContext): *>} the function
 */
function loadTool(name) {
	if (!toolModules.has(name)) {
		const url = new URL(`static/tools/${name}.js`, import.meta.url).href;
		toolModules.set(name, import(url).then((module) => module.default));
	}
	return toolModules.get(name);
}

/** The button that opens the agent popup, and the bridge to that popup. */
class JamesWidget extends HTMLElement {
	/** The popup window, null while none is known. */
	#popup = null;

	/** Listens for messages from the popup, bound to this widget. */
	#listener = (event) => this.#onMessage(event);

	/** The interface texts, empty until they are loaded. */
	#texts = {};

	/** The questions waiting for an answer from the popup, by question ID. */
	#asks = new Map();

	/** Counts the questions, so every one of them gets its own ID. */
	#askCount = 0;

	/** Language of the widget, from the attribute or the browser setting. */
	get #lang() {
		return normalizeLang(this.lang || navigator.language || "en");
	}

	/**
	 * Build the button, start listening for the popup and load the texts.
	 * @returns {Promise<void>} resolved once the button carries its name
	 */
	async connectedCallback() {
		if (!this.shadowRoot) this.#build();
		window.addEventListener("message", this.#listener);
		this.#texts = await loadTexts(this.#lang);
		this.#labelButton();
	}

	/** Stop listening. The popup is left open, so a chat is not interrupted. */
	disconnectedCallback() {
		window.removeEventListener("message", this.#listener);
	}

	/**
	 * Open the popup, or focus it when it is already open. A popup that this
	 * page lost track of, because the page was reloaded, is found by its name
	 * and left as it is, so a running conversation is not interrupted.
	 */
	open() {
		if (!this.#popup || this.#popup.closed) {
			this.#popup = window.open("", "james", "popup,width=480,height=720");
			if (!this.#popup) {
				window.alert(this.#t("widgetBlocked"));
				return;
			}
			if (isBlank(this.#popup)) this.#popup.location.href = popupURL;
		}
		this.#popup.focus();
	}

	/** Close the popup when it is open. */
	close() {
		if (this.#popup && !this.#popup.closed) this.#popup.close();
		this.#popup = null;
	}

	/**
	 * Look up an interface text.
	 * @param {string} key the text key
	 * @returns {string} the text, or the key while the texts are not loaded
	 */
	#t(key) {
		return this.#texts[key] ?? key;
	}

	/** Fill the shadow root with the button. */
	#build() {
		const root = this.attachShadow({ mode: "open" });
		root.adoptedStyleSheets = [sheet];

		const icon = document.createElement("img");
		icon.src = this.getAttribute("icon") || iconURL;
		icon.alt = "";

		const button = document.createElement("button");
		button.type = "button";
		button.append(icon);
		button.addEventListener("click", () => this.open());
		root.append(button);
	}

	/** Put the interface text on the button, as tooltip and accessible name. */
	#labelButton() {
		const button = this.shadowRoot.querySelector("button");
		button.title = this.#t("widgetOpen");
		button.setAttribute("aria-label", this.#t("widgetOpen"));
	}

	/**
	 * Send a message to the popup.
	 * @param {Object} message the message object
	 * @param {?Window} target the window to send to, the known popup by default
	 */
	#post(message, target = this.#popup) {
		if (!target || target.closed) return;
		target.postMessage(message, serverOrigin);
	}

	/**
	 * Handle a message from the popup.
	 * @param {MessageEvent} event the incoming message
	 */
	#onMessage(event) {
		// only the agent server, the origin the widget came from, may drive it
		if (event.origin !== serverOrigin) return;
		const data = event.data;
		if (!data || typeof data !== "object") return;
		if (event.source) this.#popup = event.source;

		if (data.type === "ready") {
			this.#post({
				type: "init",
				token: this.getAttribute("token") ?? "",
				lang: this.#lang,
				context: parseJSON(this.getAttribute("context"))
			}, event.source);
			return;
		}
		if (data.type === "confirm_result") {
			const ask = this.#asks.get(data.id);
			if (ask) {
				this.#asks.delete(data.id);
				ask.answer(!!data.ok);
			}
			return;
		}
		if (data.type === "tool") this.#runTool(data, event.source);
	}

	/**
	 * Run one browser tool and answer with its result. A tool that is not
	 * configured, cannot be loaded or throws answers with an error.
	 * @param {{id: string, name: string, input: Object}} call the tool call
	 * @param {?Window} source the window that asked
	 * @returns {Promise<void>} resolved once the answer is on its way
	 */
	async #runTool(call, source) {
		const input = call.input && typeof call.input === "object" ? call.input : {};
		try {
			if (!toolNames.includes(call.name)) throw new Error(`unknown tool: ${call.name}`);
			const run = await loadTool(call.name);
			const result = await run(input, this.#toolContext(call, source));
			// a module that does not await its question never counts as agreed
			if (this.#dropAsks(call.id)) throw new Error("the tool did not wait for the answer");
			const { output, after } = typeof result === "string" ? { output: result } : result ?? {};
			this.#answer(call.id, String(output ?? ""), false, source);
			// a tool may leave the page, so it acts once the answer is out
			if (after) setTimeout(after);
		} catch (error) {
			this.#dropAsks(call.id);
			this.#answer(call.id, error.message || String(error), true, source);
		}
	}

	/**
	 * Build what a tool module gets besides its input.
	 * @param {{id: string}} call the tool call being run
	 * @param {?Window} source the window that asked
	 * @returns {ToolContext} texts, the question and this element
	 */
	#toolContext(call, source) {
		return {
			t: (key) => this.#t(key),
			confirm: (text) => this.#ask(text, call, source),
			element: this
		};
	}

	/**
	 * Ask the user a yes or no question. The popup shows it, because that is
	 * the window the user looks at.
	 * @param {string} text the question
	 * @param {{id: string}} call the tool call the question belongs to
	 * @param {?Window} source the window that asked
	 * @returns {Promise<boolean>} true when the user agreed
	 */
	#ask(text, call, source) {
		if (!source || source.closed) return Promise.resolve(false);
		const id = `${call.id}#${++this.#askCount}`;
		return new Promise((answer) => {
			this.#asks.set(id, { call: call.id, answer });
			this.#post({ type: "confirm", id, call: call.id, text: String(text) }, source);
		});
	}

	/**
	 * Drop the questions of a tool call that are still waiting, and answer
	 * them with a no.
	 * @param {string} call the tool call
	 * @returns {boolean} true when a question was still waiting
	 */
	#dropAsks(call) {
		let waiting = false;
		for (const [id, ask] of this.#asks) {
			if (ask.call !== call) continue;
			this.#asks.delete(id);
			ask.answer(false);
			waiting = true;
		}
		return waiting;
	}

	/**
	 * Send the result of a browser tool.
	 * @param {string} id the tool call the result belongs to
	 * @param {string} output the result text
	 * @param {boolean} isError true when the tool failed
	 * @param {?Window} source the window that asked
	 */
	#answer(id, output, isError, source) {
		this.#post({ type: "tool_result", id, output, is_error: isError }, source);
	}
}

if (!customElements.get("james-widget")) customElements.define("james-widget", JamesWidget);
