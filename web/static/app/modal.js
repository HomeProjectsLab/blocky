// modal.js — promise-based confirm/prompt + non-blocking toasts, replacing the
// native blocking dialogs. Framework-free; builds its own DOM on demand and
// appends to document.body (no shell.html change). One shared backdrop.
//
// Message/value text is always set via textContent — never innerHTML — because
// it can carry DNS/user-controlled strings (domain names, client names).

const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

let backdrop = null;      // the one shared overlay element
let activeClose = null;   // close(result) of the dialog currently open
let lastTrigger = null;   // element to restore focus to on close

function ensureBackdrop() {
    if (backdrop) return backdrop;
    backdrop = document.createElement("div");
    backdrop.className = "modal-backdrop";
    backdrop.hidden = true;
    backdrop.addEventListener("mousedown", (e) => {
        // backdrop click (not a click that started inside the dialog) = cancel
        if (e.target === backdrop && activeClose) activeClose(null);
    });
    document.body.append(backdrop);
    return backdrop;
}

// Core: show one dialog card, wire focus-trap + keys, resolve via close().
// buildBody(card, submit) fills the card and returns the element to focus.
// onResult maps a raw result (or null for cancel) to the promise value.
function openModal({ buildDialog }) {
    const bd = ensureBackdrop();
    lastTrigger = document.activeElement;

    return new Promise((resolve) => {
        const dialog = document.createElement("div");
        dialog.className = "modal";
        dialog.setAttribute("role", "dialog");
        dialog.setAttribute("aria-modal", "true");

        function close(result) {
            if (activeClose !== close) return;
            activeClose = null;
            bd.hidden = true;
            bd.innerHTML = "";
            document.removeEventListener("keydown", onKey, true);
            document.body.style.overflow = prevOverflow;
            if (lastTrigger && lastTrigger.focus) lastTrigger.focus();
            resolve(result);
        }

        // submit(value) is the OK path; cancel is close(null-equivalent).
        const { focusEl, cancel, submit } = buildDialog(dialog, close);

        function onKey(e) {
            if (e.key === "Escape") { e.preventDefault(); cancel(); return; }
            if (e.key === "Enter" && submit) {
                // Enter confirms, except inside a textarea (allow newlines).
                if (e.target.tagName === "TEXTAREA") return;
                e.preventDefault(); submit(); return;
            }
            if (e.key !== "Tab") return;
            // focus trap
            const items = [...dialog.querySelectorAll(FOCUSABLE)].filter((el) => el.offsetParent !== null);
            if (!items.length) { e.preventDefault(); return; }
            const first = items[0], last = items[items.length - 1];
            if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
            else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
        }

        const prevOverflow = document.body.style.overflow;
        document.body.style.overflow = "hidden";
        bd.innerHTML = "";
        bd.append(dialog);
        bd.hidden = false;
        activeClose = close;
        document.addEventListener("keydown", onKey, true);
        (focusEl || dialog).focus();
    });
}

// shared header + actions builder
function titleEl(text) {
    const h = document.createElement("h2");
    h.className = "modal-title";
    h.textContent = text;
    return h;
}
function bodyEl() {
    const b = document.createElement("div");
    b.className = "modal-body";
    return b;
}
function actionsEl() {
    const a = document.createElement("div");
    a.className = "modal-actions";
    return a;
}
function btn(text, cls) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = text;
    if (cls) b.className = cls;
    return b;
}

export function confirmDialog(message, opts = {}) {
    const { title = "", okText = "Confirm", cancelText = "Cancel", danger = false } = opts;
    return openModal({
        buildDialog(dialog, close) {
            if (title) dialog.append(titleEl(title));
            const body = bodyEl();
            body.textContent = message;
            dialog.append(body);

            const actions = actionsEl();
            const cancelBtn = btn(cancelText, "btn-sub");
            const okBtn = btn(okText, danger ? "btn-danger" : "");
            okBtn.classList.add("btn-primary");
            const cancel = () => close(false);
            const submit = () => close(true);
            cancelBtn.addEventListener("click", cancel);
            okBtn.addEventListener("click", submit);
            actions.append(cancelBtn, okBtn);
            dialog.append(actions);
            return { focusEl: okBtn, cancel, submit };
        },
    });
}

export function promptDialog(label, opts = {}) {
    const { title = "", value = "", placeholder = "", okText = "OK" } = opts;
    return openModal({
        buildDialog(dialog, close) {
            if (title) dialog.append(titleEl(title));
            const body = bodyEl();
            const lab = document.createElement("label");
            lab.className = "modal-label";
            lab.textContent = label;
            const input = document.createElement("input");
            input.type = "text";
            input.value = value;
            input.placeholder = placeholder;
            const id = "modal-in-" + Math.random().toString(36).slice(2);
            input.id = id;
            lab.htmlFor = id;
            body.append(lab, input);
            dialog.append(body);

            const actions = actionsEl();
            const cancelBtn = btn("Cancel", "btn-sub");
            const okBtn = btn(okText, "btn-primary");
            const cancel = () => close(null);
            const submit = () => close(input.value);
            cancelBtn.addEventListener("click", cancel);
            okBtn.addEventListener("click", submit);
            actions.append(cancelBtn, okBtn);
            dialog.append(actions);
            return { focusEl: input, cancel, submit };
        },
    });
}

// ---- toasts: non-blocking, auto-dismiss, top-right stack ----
let toastStack = null;
function ensureStack() {
    if (toastStack) return toastStack;
    toastStack = document.createElement("div");
    toastStack.className = "toast-stack";
    toastStack.setAttribute("role", "status");
    toastStack.setAttribute("aria-live", "polite");
    document.body.append(toastStack);
    return toastStack;
}

export function toast(message, opts = {}) {
    const { type = "info", timeout = 3500 } = opts;
    const stack = ensureStack();
    const el = document.createElement("div");
    el.className = "toast toast-" + type;
    el.textContent = message;
    const remove = () => { el.classList.add("toast-out"); setTimeout(() => el.remove(), 200); };
    el.addEventListener("click", remove);
    stack.append(el);
    if (timeout > 0) setTimeout(remove, timeout);
    return remove;
}
