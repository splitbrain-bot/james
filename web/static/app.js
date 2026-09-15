/**
 * The james popup. It runs the chat against the server and shows the
 * conversation. Its only link to the host page is postMessage.
 */

/** Text that replaces an image once its turn is answered. */
const IMAGE_NOTE = "[image attached here, no longer available]";

/** Time the popup waits for one browser tool, in milliseconds. */
const TOOL_TIMEOUT = 60000;

/** Time a short note stays on screen, in milliseconds. */
const NOTE_TIMEOUT = 4000;

/** Time after which a popup still waiting for the host explains itself. */
const WAIT_HINT_TIMEOUT = 5000;

/** Pause between two ready messages while the popup waits for the host. */
const READY_INTERVAL = 500;

/**
 * Read the settings the server wrote into the page.
 * @returns {Object} the allowed origins and the image size limit
 */
function readConfig() {
	const tag = document.getElementById("james-config");
	if (!tag) return {};
	try {
		return JSON.parse(tag.textContent) ?? {};
	} catch {
		return {};
	}
}

/** Settings the server wrote into the page. */
const config = readConfig();

/** Everything the popup keeps while it is open. */
const state = {
	token: "",
	lang: "en",
	context: null,
	hostOrigin: null,
	sub: "",
	messages: [],
	attachments: [],
	abort: null,
	started: false,
	error: null,
	texts: {},
	fallback: {},
	pending: new Map(),
	nextID: 0,
	charts: [],
	generation: 0,
	storeFailed: false,
	onInit: null
};

/** The parts of the page the popup works with. */
const el = {
	messages: document.getElementById("messages"),
	note: document.getElementById("note"),
	composer: document.getElementById("composer"),
	input: document.getElementById("input"),
	files: document.getElementById("files"),
	attach: document.getElementById("attach"),
	attachments: document.getElementById("attachments"),
	send: document.getElementById("send"),
	stop: document.getElementById("stop"),
	reset: document.getElementById("reset"),
	ask: document.getElementById("ask"),
	askText: document.getElementById("ask-text"),
	askYes: document.getElementById("ask-yes"),
	askNo: document.getElementById("ask-no")
};

// ---------------------------------------------------------------- texts

/**
 * Look up an interface text.
 * @param {string} key the text key
 * @param {Object} [vars] values for {name} placeholders
 * @returns {string} the text in the chosen language, the English text, or the key
 */
function t(key, vars) {
	const text = state.texts[key] ?? state.fallback[key] ?? key;
	if (!vars) return text;
	return text.replace(/\{(\w+)\}/g, (all, name) => (name in vars ? String(vars[name]) : all));
}

/**
 * Load the interface texts of one language and the English fallback.
 * @param {string} lang the language code
 * @returns {Promise<void>} resolved once the texts are in place
 */
async function loadTexts(lang) {
	const english = fetchJSON("static/i18n/en.json");
	const chosen = lang === "en" ? {} : fetchJSON(`static/i18n/${lang}.json`).catch(() => ({}));
	state.texts = await chosen;
	state.fallback = (await english) ?? {};
}

/**
 * Fetch and parse a JSON file.
 * @param {string} url the file to read
 * @returns {Promise<*>} the parsed content
 */
async function fetchJSON(url) {
	const response = await fetch(url);
	if (!response.ok) throw new Error(`${url}: ${response.status}`);
	return response.json();
}

/**
 * Reduce a language tag to a language the popup ships.
 * @param {string} tag a language tag such as "de-AT"
 * @returns {string} "de" or "en"
 */
function normalizeLang(tag) {
	return String(tag ?? "").slice(0, 2).toLowerCase() === "de" ? "de" : "en";
}

/** Put the loaded texts on the fixed parts of the interface. */
function applyTexts() {
	document.documentElement.lang = state.lang;
	el.input.placeholder = t("placeholder");
	el.input.setAttribute("aria-label", t("placeholder"));
	label(el.send, t("send"));
	label(el.stop, t("stop"));
	label(el.attach, t("attach"));
	el.reset.textContent = t("reset");
	el.askYes.textContent = t("confirmYes");
	el.askNo.textContent = t("confirmNo");
}

/**
 * Name a button that shows only a sign.
 * @param {HTMLElement} button the button
 * @param {string} text the name of the button
 */
function label(button, text) {
	button.title = text;
	button.setAttribute("aria-label", text);
}

// ---------------------------------------------------------- host bridge

/**
 * Report whether an origin may talk to this popup. The server's own origin
 * counts as allowed, because a host page may live there.
 * @param {string} origin the origin of an incoming message
 * @returns {boolean} true when the origin is allowed
 */
function isAllowed(origin) {
	return origin === location.origin || (config.allowedOrigins ?? []).includes(origin);
}

/**
 * Handle one message from the host page. Only the window that opened the
 * popup is heard, and everything about a tool call only from the origin that
 * sent init.
 * @param {MessageEvent} event the incoming message
 */
function onMessage(event) {
	if (event.source !== window.opener || !isAllowed(event.origin)) return;
	const data = event.data;
	if (!data || typeof data !== "object") return;

	if (data.type === "init") {
		onInit(event.origin, data);
		return;
	}
	if (event.origin !== state.hostOrigin) return;
	if (data.type === "tool_result") {
		state.pending.get(data.id)?.result(data);
		return;
	}
	if (data.type === "confirm") {
		const call = state.pending.get(data.call);
		// a question whose tool call is gone can no longer be answered
		if (call) call.ask(String(data.id), String(data.text ?? ""));
		else postToHost({ type: "confirm_result", id: data.id, ok: false });
	}
}

/**
 * Take token, language and context from the host page and show the chat.
 * @param {string} origin the origin the message came from
 * @param {{token: string, lang: string, context: *}} data the init message
 * @returns {Promise<void>} resolved once the chat is on screen
 */
async function onInit(origin, data) {
	state.hostOrigin = origin;
	state.token = data.token ?? "";
	state.context = data.context;

	const lang = normalizeLang(data.lang || navigator.language);
	const sub = subjectOf(state.token);
	const changed = !state.started || sub !== state.sub;
	state.sub = sub;
	state.started = true;

	const ready = lang === state.lang ? null : loadTexts(lang);
	state.lang = lang;
	await ready;

	applyTexts();
	if (changed) state.messages = loadHistory();
	el.note.hidden = true;
	el.composer.hidden = false;
	render();
	el.input.focus();
	state.onInit?.();
}

/**
 * Report whether the host page is still there to talk to.
 * @returns {boolean} true when the opener window exists and is open
 */
function hostPresent() {
	return !!window.opener && !window.opener.closed;
}

/**
 * Send a message to the host page.
 * @param {Object} message the message object
 * @returns {boolean} false when there is no host page to send to
 */
function postToHost(message) {
	if (!hostPresent() || !state.hostOrigin) return false;
	window.opener.postMessage(message, state.hostOrigin);
	return true;
}

/**
 * Ask the host page for its token and settings. The host origin may be
 * unknown, so the message goes to every origin the popup accepts. It carries
 * nothing secret.
 */
function postReady() {
	if (!hostPresent()) return;
	for (const origin of [...(config.allowedOrigins ?? []), location.origin]) {
		window.opener.postMessage({ type: "ready" }, origin);
	}
}

/**
 * Wait until the host page sends init again, for example after it moved to a
 * new address. Ready is posted until the answer comes, at most for the time a
 * browser tool may take.
 * @returns {Promise<void>} resolved on init, or when the wait ran out
 */
function awaitHost() {
	return new Promise((resolve) => {
		const timer = setInterval(postReady, READY_INTERVAL);
		const limit = setTimeout(done, TOOL_TIMEOUT);

		/** End the wait once. */
		function done() {
			clearInterval(timer);
			clearTimeout(limit);
			state.onInit = null;
			resolve();
		}

		state.onInit = done;
		postReady();
	});
}

/**
 * Read the subject claim of a token. The token is not verified here, the
 * subject only keys the stored history.
 * @param {string} token the JWT from the host page
 * @returns {string} the subject, or an empty string when it cannot be read
 */
function subjectOf(token) {
	const payload = String(token).split(".")[1];
	if (!payload) return "";
	try {
		const raw = atob(payload.replace(/-/g, "+").replace(/_/g, "/"));
		const bytes = Uint8Array.from(raw, (char) => char.charCodeAt(0));
		return String(JSON.parse(new TextDecoder().decode(bytes)).sub ?? "");
	} catch {
		return "";
	}
}

// -------------------------------------------------------------- history

/**
 * The localStorage key of the current user's history.
 * @returns {string} the key
 */
function historyKey() {
	return `james:history:${state.sub}`;
}

/**
 * Read the stored history of the current user. Without a user ID nothing is
 * stored, so nothing is read.
 * @returns {Array<Object>} the messages, empty when there are none
 */
function loadHistory() {
	if (!state.sub) return [];
	try {
		const stored = JSON.parse(localStorage.getItem(historyKey()) ?? "[]");
		return Array.isArray(stored) ? stored : [];
	} catch {
		return [];
	}
}

/**
 * Store the history of the current user, with a note in place of every image.
 * Without a user ID the history stays in memory. A failing store is reported
 * once and does not break the conversation.
 */
function saveHistory() {
	if (!state.sub) return;
	try {
		localStorage.setItem(historyKey(), JSON.stringify(withoutImages(state.messages)));
	} catch {
		if (!state.storeFailed) {
			state.storeFailed = true;
			showNote(t("storageFailed"));
		}
	}
}

/**
 * Drop the history of the current user and clear the view. A running turn is
 * stopped first, so its answer cannot land in the fresh conversation.
 */
function resetHistory() {
	state.generation++;
	state.abort?.abort();
	state.messages = [];
	state.error = null;
	if (state.sub) {
		try {
			localStorage.removeItem(historyKey());
		} catch {
			// without a store there is nothing to remove
		}
	}
	render();
}

/**
 * Copy messages with every image block replaced by a short note, in user
 * messages and in tool results alike.
 * @param {Array<Object>} messages the messages to copy
 * @returns {Array<Object>} the copies
 */
function withoutImages(messages) {
	const note = { type: "text", text: IMAGE_NOTE };
	return messages.map((message) => ({
		...message,
		content: (message.content ?? []).map((block) => {
			if (block.type === "image") return note;
			if (block.type === "tool_result" && Array.isArray(block.content)) {
				return {
					...block,
					content: block.content.map((inner) => (inner.type === "image" ? note : inner))
				};
			}
			return block;
		})
	}));
}

/** Replace every image block of the history with a short note. */
function dropImages() {
	state.messages = withoutImages(state.messages);
}

// --------------------------------------------------------------- render

/** Draw the whole conversation from the history. */
function render() {
	for (const chart of state.charts) chart.destroy();
	state.charts = [];
	el.messages.textContent = "";

	const results = collectResults();
	for (const message of state.messages) {
		const turn = renderTurn(message, results);
		if (turn) el.messages.append(turn);
	}
	for (const box of el.messages.querySelectorAll(".answer .bubble")) enhance(box);
	if (state.error) el.messages.append(errorBox(state.error.code, state.error.message));
	scrollDown();
}

/**
 * Collect all tool results of the history, keyed by the call they answer.
 * @returns {Map<string, Object>} the result block of every tool call
 */
function collectResults() {
	const results = new Map();
	for (const message of state.messages) {
		for (const block of message.content ?? []) {
			if (block.type === "tool_result") results.set(block.tool_use_id, block);
		}
	}
	return results;
}

/**
 * Draw one message of the history.
 * @param {Object} message the message
 * @param {Map<string, Object>} results the tool results of the conversation
 * @returns {?HTMLElement} the element, or null when there is nothing to show
 */
function renderTurn(message, results) {
	const turn = document.createElement("div");
	turn.className = `turn ${message.role === "user" ? "user" : "assistant"}`;
	const images = [];
	let text = "";
	let empty = true;

	/** Put the collected text into a bubble, so the order of blocks stays. */
	function flushText() {
		if (text) turn.append(bubble(message.role, text));
		text = "";
	}

	for (const block of message.content ?? []) {
		if (block.type === "text") {
			text += (text ? "\n\n" : "") + block.text;
			empty = false;
		} else if (block.type === "image") {
			images.push(block);
			empty = false;
		} else if (block.type === "tool_use") {
			flushText();
			turn.append(activityLine(block, results.get(block.id)));
			empty = false;
		}
	}

	if (images.length) turn.prepend(thumbnails(images));
	flushText();
	return empty ? null : turn;
}

/**
 * Build a message bubble.
 * @param {string} role who wrote the message
 * @param {string} text the text, markdown for the assistant
 * @returns {HTMLElement} the bubble, with a copy button for answers
 */
function bubble(role, text) {
	const box = document.createElement("div");
	box.className = "bubble";
	if (role === "user") {
		renderPlain(box, text);
		return box;
	}
	renderMarkdown(box, text);

	const wrap = document.createElement("div");
	wrap.className = "answer";
	wrap.append(box, copyButton(text));
	return wrap;
}

/**
 * Show plain text, with the image note in its own style.
 * @param {HTMLElement} box the element to fill
 * @param {string} text the text to show
 */
function renderPlain(box, text) {
	text.split(IMAGE_NOTE).forEach((part, index) => {
		if (index > 0) {
			const note = document.createElement("div");
			note.className = "image-note";
			note.textContent = t("imageNote");
			box.append(note);
		}
		if (part.trim()) {
			const line = document.createElement("div");
			line.textContent = part.trim();
			box.append(line);
		}
	});
}

/**
 * Render markdown into an element. The result is sanitised, because the text
 * comes from the model and may contain anything.
 * @param {HTMLElement} box the element to fill
 * @param {string} text the markdown
 */
function renderMarkdown(box, text) {
	const html = window.marked.parse(text, { gfm: true, breaks: false });
	box.innerHTML = window.DOMPurify.sanitize(html);
}

/**
 * Build the button that copies an answer as markdown.
 * @param {string} text the raw markdown of the answer
 * @returns {HTMLElement} the button
 */
function copyButton(text) {
	const button = document.createElement("button");
	button.type = "button";
	button.className = "copy-button";
	button.textContent = t("copy");
	button.addEventListener("click", async () => {
		try {
			await navigator.clipboard.writeText(text);
		} catch {
			// the browser refused the clipboard, there is nothing to report
			return;
		}
		button.textContent = t("copied");
		setTimeout(() => {
			button.textContent = t("copy");
		}, 2000);
	});
	return button;
}

/**
 * Build the thumbnails of the images of one message.
 * @param {Array<Object>} images the image blocks
 * @returns {HTMLElement} the thumbnail row
 */
function thumbnails(images) {
	const row = document.createElement("div");
	row.className = "thumbs";
	for (const image of images) {
		const thumb = document.createElement("div");
		thumb.className = "thumb";
		thumb.append(imageTag(image));
		row.append(thumb);
	}
	return row;
}

/**
 * Build the picture of one image block.
 * @param {{media_type: string, data: string}} image the image block
 * @returns {HTMLImageElement} the picture
 */
function imageTag(image) {
	const img = document.createElement("img");
	img.src = `data:${image.media_type};base64,${image.data}`;
	img.alt = "";
	return img;
}

/**
 * Build the collapsible line of one tool call.
 * @param {Object} call the tool_use block
 * @param {Object} [result] the tool_result block that answers it
 * @returns {HTMLElement} the activity line
 */
function activityLine(call, result) {
	const line = document.createElement("details");
	line.className = "activity";

	const head = document.createElement("summary");
	const body = document.createElement("pre");
	body.textContent = summarize(call.input, 400);
	line.append(head, body);

	setActivity(line, call.name, result ? resultText(result) : null, result?.is_error);
	return line;
}

/**
 * Set the state of an activity line.
 * @param {HTMLElement} line the activity line
 * @param {string} name the tool name
 * @param {?string} output the result text, null while the tool runs
 * @param {boolean} [isError] true when the tool failed
 */
function setActivity(line, name, output, isError) {
	const key = output === null ? "toolRunning" : isError ? "toolFailed" : "toolDone";
	line.querySelector("summary").textContent = t(key, { tool: name });
	line.classList.toggle("failed", !!isError);
	if (output !== null) {
		const body = line.querySelector("pre");
		body.textContent += `\n\n${summarize(output, 1000)}`;
	}
}

/**
 * Ask the user a yes or no question in the sheet at the bottom of the popup.
 * A question that still stands is answered with a no first, so only one is
 * ever on screen. Anything but a yes counts as a no, the escape key included.
 * @param {string} text the question
 * @returns {Promise<boolean>} true when the user agreed
 */
function ask(text) {
	el.ask.returnValue = "";
	closeAsk();
	el.askText.textContent = text;
	el.ask.showModal();
	return new Promise((answer) => {
		el.ask.addEventListener("close", () => answer(el.ask.returnValue === "yes"), { once: true });
	});
}

/** Take back the question on screen, which answers it with a no. */
function closeAsk() {
	if (el.ask.open) el.ask.close("no");
}

/**
 * Take the text out of a tool result block.
 * @param {Object} result the tool_result block
 * @returns {string} the text of the result
 */
function resultText(result) {
	if (typeof result.content === "string") return result.content;
	return (result.content ?? [])
		.map((block) => (block.type === "text" ? block.text : `[${block.type}]`))
		.join("\n");
}

/**
 * Shorten a value for an activity line.
 * @param {*} value the value to show
 * @param {number} max the largest number of characters
 * @returns {string} the shortened text
 */
function summarize(value, max) {
	const text = (typeof value === "string" ? value : JSON.stringify(value)) ?? "";
	return text.length > max ? `${text.slice(0, max)}…` : text;
}

/**
 * Build the sign that the popup waits for an answer. It carries no text,
 * because the conversation is a live region and dots read as nothing.
 * @returns {HTMLElement} the three dots
 */
function thinking() {
	const dots = document.createElement("div");
	dots.className = "thinking";
	dots.setAttribute("aria-hidden", "true");
	for (let i = 0; i < 3; i++) dots.append(document.createElement("span"));
	return dots;
}

/** Scroll the conversation to its end. */
function scrollDown() {
	el.messages.scrollTop = el.messages.scrollHeight;
}

/**
 * Show a short note above the composer.
 * @param {string} text the note
 */
function showNote(text) {
	el.note.textContent = text;
	el.note.hidden = false;
}

/**
 * Show a note for a few seconds. A note that replaced it in the meantime
 * stays.
 * @param {string} text the note
 */
function flashNote(text) {
	showNote(text);
	setTimeout(() => {
		if (el.note.textContent === text) el.note.hidden = true;
	}, NOTE_TIMEOUT);
}

/**
 * Show an error at the end of the conversation.
 * @param {string} code the error code from the server
 * @param {string} message the error message
 */
function showError(code, message) {
	state.error = { code, message };
	el.messages.append(errorBox(code, message));
	scrollDown();
}

/**
 * Build the box of an error. A code the texts know is shown in the user's
 * language, any other code with the message the server sent.
 * @param {string} code the error code from the server
 * @param {string} message the error message
 * @returns {HTMLElement} the box
 */
function errorBox(code, message) {
	const box = document.createElement("div");
	box.className = "error";
	const line = document.createElement("div");
	box.append(line);

	if (code === "context_too_long") {
		line.textContent = t("contextTooLong");
		const button = document.createElement("button");
		button.type = "button";
		button.className = "icon-button";
		button.textContent = t("newConversation");
		button.addEventListener("click", resetHistory);
		box.append(button);
	} else {
		const key = `error_${code}`;
		const known = state.texts[key] ?? state.fallback[key];
		line.textContent = `${t("errorPrefix")}: ${known ?? message}`;
	}
	return box;
}

// ----------------------------------------------------- diagrams, charts

/**
 * Draw the mermaid and chart blocks of a finished answer.
 * @param {HTMLElement} box the rendered answer
 */
function enhance(box) {
	const diagrams = box.querySelectorAll("pre > code.language-mermaid");
	const charts = box.querySelectorAll("pre > code.language-chart");
	if (diagrams.length) drawDiagrams(diagrams);
	if (charts.length) drawCharts(charts);
}

/**
 * Load mermaid and draw the diagram blocks of one answer.
 * @param {NodeList} diagrams the code elements holding the diagram sources
 * @returns {Promise<void>} resolved once every diagram was tried
 */
async function drawDiagrams(diagrams) {
	try {
		await loadScript("static/vendor/mermaid.min.js");
	} catch {
		for (const code of diagrams) failBlock(code, t("diagramError"));
		return;
	}
	window.mermaid.initialize({
		startOnLoad: false,
		securityLevel: "strict",
		theme: "neutral",
		// labels as SVG text, because the sanitiser drops foreignObject
		htmlLabels: false,
		flowchart: { htmlLabels: false }
	});
	await Promise.all([...diagrams].map(drawDiagram));
}

/**
 * Load uPlot and draw the chart blocks of one answer.
 * @param {NodeList} charts the code elements holding the chart specifications
 * @returns {Promise<void>} resolved once every chart was tried
 */
async function drawCharts(charts) {
	loadStyle("static/vendor/uPlot.min.css");
	try {
		await loadScript("static/vendor/uPlot.iife.min.js");
	} catch {
		for (const code of charts) failBlock(code, t("chartError"));
		return;
	}
	for (const code of charts) drawChart(code);
}

/**
 * Keep a code block and show a message below it.
 * @param {HTMLElement} code the code element of the block
 * @param {string} message the message to show below it
 */
function failBlock(code, message) {
	const pre = code.parentNode;
	if (!pre?.parentNode) return;
	const note = document.createElement("div");
	note.className = "image-note";
	note.textContent = message;
	pre.after(note);
}

/**
 * Draw one mermaid block in place of its code block.
 * @param {HTMLElement} code the code element holding the diagram source
 * @returns {Promise<void>} resolved once the diagram is drawn or has failed
 */
async function drawDiagram(code) {
	const pre = code.parentNode;
	const id = `james-diagram-${state.nextID++}`;
	try {
		const { svg } = await window.mermaid.render(id, code.textContent);
		const box = document.createElement("div");
		box.className = "diagram";
		box.innerHTML = window.DOMPurify.sanitize(svg);
		pre.replaceWith(box);
	} catch (error) {
		failBlock(code, `${t("diagramError")} ${error?.message ?? ""}`);
	} finally {
		// mermaid renders into a temporary element that it leaves behind
		document.getElementById(`d${id}`)?.remove();
	}
}

/**
 * Draw one chart block in place of its code block.
 * @param {HTMLElement} code the code element holding the chart JSON
 */
function drawChart(code) {
	const pre = code.parentNode;
	let spec;
	try {
		spec = parseChart(JSON.parse(code.textContent));
	} catch {
		failBlock(code, t("chartError"));
		return;
	}

	const box = document.createElement("div");
	box.className = "chart";
	pre.replaceWith(box);

	const colors = seriesColors(spec.series.length);
	const options = {
		title: spec.title,
		width: Math.max(box.clientWidth || 320, 240),
		height: 240,
		scales: { x: { time: false, range: padRange } },
		axes: [
			{
				label: spec.xLabel,
				splits: wholeSplits,
				values: (chart, splits) => splits.map((value) => spec.labels[value] ?? "")
			},
			{ label: spec.yLabel }
		],
		series: [
			{ label: spec.xLabel, value: (chart, value) => spec.labels[value] ?? "" },
			...spec.series.map((one, index) => ({
				label: one.label,
				stroke: colors[index],
				width: 2,
				fill: spec.type === "bar" ? colors[index] : undefined,
				points: { show: spec.type !== "bar" },
				paths: spec.type === "bar" ? barPaths(index, spec.series.length) : undefined
			}))
		]
	};
	const data = [spec.x, ...spec.series.map((one) => one.values)];

	try {
		state.charts.push(new window.uPlot(options, data, box));
	} catch {
		box.textContent = t("chartError");
	}
}

/**
 * Check a chart specification and put it in the form uPlot needs.
 * @param {Object} spec the parsed JSON of a chart block
 * @returns {Object} the checked specification
 */
function parseChart(spec) {
	if (!spec || typeof spec !== "object") throw new Error("not an object");
	const labels = spec.x?.values ?? [];
	const series = spec.series;
	if (!Array.isArray(labels) || !labels.length) throw new Error("no x values");
	if (!Array.isArray(series) || !series.length) throw new Error("no series");
	for (const one of series) {
		if (!one || !Array.isArray(one.values) || one.values.length !== labels.length) {
			throw new Error("series and x values do not match");
		}
	}
	return {
		title: String(spec.title ?? ""),
		type: spec.type === "bar" ? "bar" : "line",
		labels: labels.map(String),
		x: labels.map((value, index) => index),
		xLabel: String(spec.x?.label ?? ""),
		yLabel: String(spec.y?.label ?? ""),
		series: series.map((one, index) => ({
			label: String(one.label || index + 1),
			values: one.values.map((value) => (value === null || value === undefined ? null : Number(value)))
		}))
	};
}

/**
 * Widen the x range by half a step, so the first and the last bar fit.
 * @param {Object} chart the chart
 * @param {number} min the smallest x value
 * @param {number} max the largest x value
 * @returns {Array<number>} the range to show
 */
function padRange(chart, min, max) {
	return [min - 0.5, max + 0.5];
}

/**
 * Place the ticks of the x axis on whole positions, one per label.
 * @param {Object} chart the chart
 * @param {number} axis the axis index
 * @param {number} min the first shown value
 * @param {number} max the last shown value
 * @returns {Array<number>} the tick positions
 */
function wholeSplits(chart, axis, min, max) {
	const ticks = [];
	for (let i = Math.ceil(min); i <= Math.floor(max); i++) ticks.push(i);
	return ticks;
}

/**
 * Build the bar drawing of one series, so bars that share an x position stand
 * next to each other.
 * @param {number} index the position of the series
 * @param {number} count how many series the chart has
 * @returns {Function} the paths builder for uPlot
 */
function barPaths(index, count) {
	const group = 0.8;
	const width = group / count;
	return window.uPlot.paths.bars({
		size: [1, Infinity],
		align: 1,
		gap: 1,
		disp: {
			x0: {
				unit: 1,
				values: (chart) => chart.data[0].map((x) => x - group / 2 + index * width)
			},
			size: {
				unit: 1,
				values: () => [width]
			}
		}
	});
}

/**
 * Take the series colours from the style sheet, so they follow the theme.
 * @param {number} count how many colours are needed
 * @returns {Array<string>} the colours, repeated after the eighth
 */
function seriesColors(count) {
	const style = getComputedStyle(document.documentElement);
	return Array.from({ length: count }, (ignored, index) =>
		style.getPropertyValue(`--series-${(index % 8) + 1}`).trim() || "#2a78d6");
}

/** The scripts loaded on demand, by URL. */
const scripts = new Map();

/**
 * Load a script once.
 * @param {string} src the script URL
 * @returns {Promise<void>} resolved once the script ran
 */
function loadScript(src) {
	if (!scripts.has(src)) {
		scripts.set(src, new Promise((resolve, reject) => {
			const tag = document.createElement("script");
			tag.src = src;
			tag.addEventListener("load", () => resolve());
			tag.addEventListener("error", () => {
				// the next block tries again
				scripts.delete(src);
				reject(new Error(`cannot load ${src}`));
			});
			document.head.append(tag);
		}));
	}
	return scripts.get(src);
}

/**
 * Load a style sheet once.
 * @param {string} href the style sheet URL
 */
function loadStyle(href) {
	if (document.querySelector(`link[href="${href}"]`)) return;
	const tag = document.createElement("link");
	tag.rel = "stylesheet";
	tag.href = href;
	document.head.append(tag);
}

// ------------------------------------------------------------ composer

/**
 * Take images from a file list into the pending attachments.
 * @param {FileList|Array<File>} files the chosen files
 */
function addFiles(files) {
	const max = config.maxImageBytes ?? 0;
	for (const file of files) {
		if (!file.type?.startsWith("image/")) continue;
		if (max && file.size > max) {
			flashNote(t("imageTooLarge", { max: formatBytes(max) }));
			continue;
		}
		attachImage(file);
	}
}

/**
 * Read one image and show it among the pending attachments.
 * @param {File} file the image
 * @returns {Promise<void>} resolved once the image is read or has failed
 */
async function attachImage(file) {
	try {
		state.attachments.push(await readImage(file));
	} catch (error) {
		showNote(`${t("errorPrefix")}: ${error?.message ?? String(error)}`);
		return;
	}
	renderAttachments();
}

/**
 * Read one image file as base64.
 * @param {File} file the image
 * @returns {Promise<{media_type: string, data: string}>} the image block
 */
function readImage(file) {
	return new Promise((resolve, reject) => {
		const reader = new FileReader();
		reader.addEventListener("load", () => {
			resolve({ media_type: file.type, data: String(reader.result).split(",")[1] ?? "" });
		});
		reader.addEventListener("error", () => reject(reader.error));
		reader.readAsDataURL(file);
	});
}

/**
 * Write a byte count in a short form.
 * @param {number} bytes the number of bytes
 * @returns {string} the size with its unit
 */
function formatBytes(bytes) {
	if (bytes >= 1048576) return `${Math.round(bytes / 1048576)} MB`;
	if (bytes >= 1024) return `${Math.round(bytes / 1024)} kB`;
	return `${bytes} B`;
}

/** Draw the thumbnails of the attachments that are waiting to be sent. */
function renderAttachments() {
	el.attachments.textContent = "";
	el.attachments.hidden = state.attachments.length === 0;
	state.attachments.forEach((image, index) => {
		const remove = document.createElement("button");
		remove.type = "button";
		remove.textContent = "×";
		label(remove, t("remove"));
		remove.addEventListener("click", () => {
			state.attachments.splice(index, 1);
			renderAttachments();
		});

		const thumb = document.createElement("div");
		thumb.className = "thumb";
		thumb.append(imageTag(image), remove);
		el.attachments.append(thumb);
	});
}

/**
 * Show or hide the buttons that belong to a running turn.
 * @param {boolean} busy true while a turn runs
 */
function setBusy(busy) {
	el.send.hidden = busy;
	el.stop.hidden = !busy;
	el.attach.disabled = busy;
}

/** Send the text and the attachments of the composer as a new turn. */
function submit() {
	if (state.abort) {
		flashNote(t("busy"));
		return;
	}
	const text = el.input.value.trim();
	if (!text && !state.attachments.length) return;
	state.error = null;

	const content = [
		...(text ? [{ type: "text", text }] : []),
		...state.attachments.map((image) => ({ type: "image", ...image }))
	];
	state.messages.push({ role: "user", content });
	state.attachments = [];
	el.input.value = "";
	el.input.style.height = "";
	renderAttachments();
	saveHistory();
	render();
	runTurn();
}

// ----------------------------------------------------------- one turn

/**
 * Run turns against the server until no browser tool is left.
 * @returns {Promise<void>} resolved once the last turn ended
 */
async function runTurn() {
	state.error = null;
	const controller = new AbortController();
	const generation = state.generation;
	state.abort = controller;
	setBusy(true);

	let answered = false;
	try {
		answered = await step(controller);
	} catch (error) {
		if (error.name !== "AbortError") showError("internal", error.message || String(error));
	}

	state.abort = null;
	setBusy(false);
	if (generation !== state.generation) return;
	// an image the model has seen is not sent again
	if (answered) dropImages();
	saveHistory();
	render();
}

/**
 * Run one turn and, when browser tools are asked for, the next one. A turn
 * whose conversation was reset in the meantime is dropped.
 * @param {AbortController} controller aborts the running fetch
 * @returns {Promise<boolean>} true when the model answered at least once
 */
async function step(controller) {
	const generation = state.generation;
	const done = await streamTurn(controller.signal);
	if (!done || generation !== state.generation) return false;

	state.messages = state.messages.concat(done.messages ?? []);
	saveHistory();
	render();
	if (done.stop_reason === "max_tokens") flashNote(t("answerCut"));
	if (!done.browser_tools?.length) return true;

	const results = await runBrowserTools(done.browser_tools, controller.signal);
	if (generation !== state.generation) return true;
	state.messages.push({ role: "user", content: [...(done.tool_results ?? []), ...results] });
	saveHistory();
	render();
	await step(controller);
	return true;
}

/**
 * Post the history and show the streamed answer. A stream that ends without a
 * done or error event is reported as an error, and the text it brought is kept
 * as an answer.
 * @param {AbortSignal} signal aborts the fetch
 * @returns {Promise<?Object>} the data of the done event, null after an error
 */
async function streamTurn(signal) {
	const live = document.createElement("div");
	live.className = "turn assistant";
	// the dots stand below the turn, so they stay under what arrives
	const dots = thinking();
	el.messages.append(live, dots);
	scrollDown();

	const lines = new Map();
	const answer = { box: null, text: "", segment: "", failed: false, frame: 0 };
	let result = null;

	try {
		const response = await fetch("chat", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				token: state.token,
				lang: state.lang,
				context: state.context,
				messages: state.messages
			}),
			signal
		});
		const type = response.headers.get("Content-Type") ?? "";
		if (!response.ok && !type.includes("text/event-stream")) {
			answer.failed = true;
			showError("internal", (await response.text()) || String(response.status));
		} else {
			await readEvents(response, (name, data) => {
				result = handleEvent(name, data, live, lines, answer) ?? result;
			});
		}
	} finally {
		live.remove();
		dots.remove();
	}

	if (!result && !answer.failed) {
		if (answer.text) {
			state.messages.push({ role: "assistant", content: [{ type: "text", text: answer.text }] });
			render();
		}
		showError("stream_ended", t("error_stream_ended"));
	}
	return result;
}

/**
 * Act on one streamed event.
 * @param {string} name the event name
 * @param {Object} data the event data
 * @param {HTMLElement} live the element of the running turn
 * @param {Map<string, HTMLElement>} lines the activity lines of this turn
 * @param {{box: ?HTMLElement, text: string, segment: string, failed: boolean, frame: number}} answer the answer being written
 * @returns {?Object} the data of a done event, null otherwise
 */
function handleEvent(name, data, live, lines, answer) {
	if (name === "text") {
		if (!answer.box) {
			answer.box = document.createElement("div");
			answer.box.className = "bubble";
			answer.segment = "";
			live.append(answer.box);
		}
		answer.text += data.text ?? "";
		answer.segment += data.text ?? "";
		scheduleMarkdown(answer);
		return null;
	}
	if (name === "tool_start") {
		const line = activityLine({ name: data.name, input: data.input }, null);
		lines.set(data.id, line);
		live.append(line);
		// text after a tool call gets a bubble of its own, as in the history
		answer.box = null;
		scrollDown();
		return null;
	}
	if (name === "tool_end") {
		const known = lines.get(data.id);
		if (known) setActivity(known, data.name, data.output ?? "", data.is_error);
		return null;
	}
	if (name === "error") {
		answer.failed = true;
		// a token that ran out is replaced by the host page for the next try
		if (data.code === "unauthorized") postReady();
		showError(data.code, data.message ?? "");
		return null;
	}
	return name === "done" ? data : null;
}

/**
 * Draw the text of the current bubble as markdown, at most once per frame.
 * @param {{box: ?HTMLElement, segment: string, frame: number}} answer the answer being written
 */
function scheduleMarkdown(answer) {
	if (answer.frame) return;
	answer.frame = requestAnimationFrame(() => {
		answer.frame = 0;
		if (answer.box) renderMarkdown(answer.box, answer.segment);
		scrollDown();
	});
}

/**
 * Read a Server-Sent Events stream and hand every event on.
 * @param {Response} response the streaming response
 * @param {Function} onEvent called with the event name and its data
 * @returns {Promise<void>} resolved when the stream ends
 */
async function readEvents(response, onEvent) {
	const reader = response.body.getReader();
	const decoder = new TextDecoder();
	let buffer = "";

	for (;;) {
		const chunk = await reader.read();
		if (chunk.done) return;
		buffer += decoder.decode(chunk.value, { stream: true });
		const blocks = buffer.split(/\r?\n\r?\n/);
		buffer = blocks.pop();
		for (const block of blocks) {
			const event = parseEvent(block);
			if (event) onEvent(event.name, event.data);
		}
	}
}

/**
 * Read one event block of a Server-Sent Events stream.
 * @param {string} block the lines of one event
 * @returns {?{name: string, data: Object}} the event, null when it is broken
 */
function parseEvent(block) {
	let name = "message";
	let data = "";
	for (const line of block.split(/\r?\n/)) {
		if (line.startsWith("event:")) name = line.slice(6).trim();
		else if (line.startsWith("data:")) data += (data ? "\n" : "") + line.slice(5).trim();
	}
	if (!data) return null;
	try {
		return { name, data: JSON.parse(data) };
	} catch {
		return null;
	}
}

// ------------------------------------------------------ browser tools

/**
 * Run the browser tools of one turn, one after the other. After the host page
 * moved to a new address, the popup waits for the new page before it goes on.
 * @param {Array<Object>} calls the tool calls from the server
 * @param {AbortSignal} signal ends the wait when the user stops the turn
 * @returns {Promise<Array<Object>>} one tool_result block per call
 */
async function runBrowserTools(calls, signal) {
	const results = [];
	for (const call of calls) {
		const result = await runBrowserTool(call, signal);
		results.push(result);
		if (call.name === "navigate" && !result.is_error) await awaitHost();
	}
	return results;
}

/**
 * Ask the host page to run one browser tool and show the questions the tool
 * asks along the way.
 * @param {{id: string, name: string, input: *}} call the tool call
 * @param {AbortSignal} signal ends the wait when the user stops the turn
 * @returns {Promise<Object>} the tool_result block for the answer
 */
function runBrowserTool(call, signal) {
	return new Promise((resolve, reject) => {
		/** True while a question of this call stands on screen. */
		let standing = false;
		let timer = 0;

		/** Start the wait for the host page. */
		function wait() {
			timer = setTimeout(() => finish(t("browserToolTimeout"), true), TOOL_TIMEOUT);
		}

		/** Stop the wait, so a question may stand as long as the user needs. */
		function hold() {
			clearTimeout(timer);
			timer = 0;
		}

		/**
		 * Put one question of the tool to the user and answer the host page.
		 * @param {string} id the question ID
		 * @param {string} text the question
		 * @returns {Promise<void>} resolved once the answer is on its way
		 */
		async function question(id, text) {
			hold();
			standing = true;
			const ok = await ask(text);
			standing = false;
			postToHost({ type: "confirm_result", id, ok });
			// a call that ended in the meantime waits for nothing any more
			if (state.pending.has(call.id)) wait();
		}

		/** Forget the call, so a late answer is ignored. */
		function cleanup() {
			if (standing) closeAsk();
			hold();
			signal.removeEventListener("abort", onAbort);
			state.pending.delete(call.id);
		}

		/**
		 * Answer the call once, with the result or with a failure.
		 * @param {string} output the result text
		 * @param {boolean} isError true when the tool failed
		 */
		function finish(output, isError) {
			cleanup();
			resolve({
				type: "tool_result",
				tool_use_id: call.id,
				content: [{ type: "text", text: output || t("toolNoOutput") }],
				is_error: !!isError
			});
		}

		/** End the wait when the user stops the turn. */
		function onAbort() {
			cleanup();
			reject(new DOMException("the turn was stopped", "AbortError"));
		}

		if (signal.aborted) return onAbort();
		signal.addEventListener("abort", onAbort);
		state.pending.set(call.id, {
			result: (message) => finish(String(message.output ?? ""), message.is_error),
			ask: question
		});
		wait();
		if (!postToHost({ type: "tool", id: call.id, name: call.name, input: call.input })) {
			finish(t("hostGone"), true);
		}
	});
}

// ------------------------------------------------------------- startup

/** Connect the buttons and the keyboard of the composer. */
function bindComposer() {
	el.composer.addEventListener("submit", (event) => {
		event.preventDefault();
		submit();
	});
	el.input.addEventListener("keydown", (event) => {
		if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
			event.preventDefault();
			submit();
		}
	});
	el.input.addEventListener("input", () => {
		el.input.style.height = "auto";
		// the height carries the border, which scrollHeight leaves out
		const border = el.input.offsetHeight - el.input.clientHeight;
		const grown = Math.min(el.input.scrollHeight + border, window.innerHeight / 3);
		el.input.style.height = `${grown}px`;
	});
	el.input.addEventListener("paste", (event) => {
		const files = event.clipboardData?.files ?? [];
		if (files.length) {
			event.preventDefault();
			addFiles(files);
		}
	});
	el.attach.addEventListener("click", () => el.files.click());
	el.files.addEventListener("change", () => {
		addFiles(el.files.files);
		el.files.value = "";
	});
	el.stop.addEventListener("click", () => state.abort?.abort());
	el.reset.addEventListener("click", async () => {
		if (await ask(t("confirmReset"))) resetHistory();
	});
	window.addEventListener("resize", () => {
		for (const chart of state.charts) {
			chart.setSize({ width: Math.max(chart.root.parentNode.clientWidth || 320, 240), height: 240 });
		}
	});
}

/** Open links of answers in a new tab, so the conversation stays. */
function bindLinks() {
	window.DOMPurify.addHook("afterSanitizeAttributes", (node) => {
		if (node.tagName === "A" && node.hasAttribute("href")) {
			node.setAttribute("target", "_blank");
			node.setAttribute("rel", "noopener noreferrer");
		}
	});
}

/** Ask the host page for the token and wait for its answer. */
function start() {
	bindComposer();
	bindLinks();
	applyTexts();
	if (!hostPresent()) {
		showNote(t("openerMissing"));
		return;
	}
	window.addEventListener("message", onMessage);
	showNote(t("waiting"));
	setTimeout(() => {
		if (!state.started) showNote(t("waitingLong"));
	}, WAIT_HINT_TIMEOUT);
	postReady();
}

state.lang = normalizeLang(navigator.language);
try {
	await loadTexts(state.lang);
} catch {
	// without the texts the popup shows the keys, it still works
}
start();
