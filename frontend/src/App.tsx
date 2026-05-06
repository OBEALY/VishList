import {
  Check,
  ChevronLeft,
  ChevronRight,
  Crown,
  ExternalLink,
  Gift,
  ImagePlus,
  LogIn,
  LogOut,
  PackagePlus,
  RefreshCw,
  Trash2,
  UserPlus,
  X
} from "lucide-react";
import { FormEvent, useEffect, useMemo, useRef, useState } from "react";

type GiftItem = {
  id: number;
  title: string;
  description: string;
  linkUrl: string;
  imageUrl: string;
  reserved: boolean;
  reservedByMe: boolean;
  createdAt: string;
  updatedAt: string;
};

type UserSession = {
  token: string;
  role: "guest" | "admin";
  username: string;
  name: string;
};

type Mode = "guest" | "admin";
type AuthMode = "login" | "register";
type Toast = { kind: "ok" | "error"; text: string } | null;

const guestStorageKey = "vislist.guestSession.v3";
const adminStorageKey = "vislist.adminSession.v3";
const birthdayPhotos = Array.from({ length: 11 }, (_, index) => {
  const number = String(index + 1).padStart(2, "0");
  return `/birthday/birthday-${number}.jpg`;
});

function App() {
  const [guestSession, setGuestSession] = useState<UserSession | null>(() =>
    readStoredSession(guestStorageKey, "guest")
  );
  const [adminSession, setAdminSession] = useState<UserSession | null>(() =>
    readStoredSession(adminStorageKey, "admin")
  );
  const [mode, setMode] = useState<Mode>(() =>
    readStoredSession(adminStorageKey, "admin") && !readStoredSession(guestStorageKey, "guest")
      ? "admin"
      : "guest"
  );
  const [gifts, setGifts] = useState<GiftItem[]>([]);
  const [reservations, setReservations] = useState<GiftItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [toast, setToast] = useState<Toast>(null);

  const reservedCount = useMemo(() => reservations.length, [reservations]);
  const needsAuth = (mode === "guest" && !guestSession) || (mode === "admin" && !adminSession);

  useEffect(() => {
    if (!guestSession && adminSession && mode !== "admin") {
      setMode("admin");
    }
  }, [guestSession?.token, adminSession?.token, mode]);

  useEffect(() => {
    if (needsAuth) return;
    void refreshGifts();
  }, [mode, guestSession?.token, adminSession?.token, needsAuth]);

  useEffect(() => {
    if (guestSession) {
      void refreshReservations(guestSession);
    } else {
      setReservations([]);
    }
  }, [guestSession?.token]);

  useEffect(() => {
    if (!toast) return;
    const timer = window.setTimeout(() => setToast(null), 3600);
    return () => window.clearTimeout(timer);
  }, [toast]);

  async function refreshGifts() {
    setLoading(true);
    try {
      if (mode === "admin") {
        const data = await requestJson<{ gifts: GiftItem[] }>("/api/admin/gifts", {
          headers: adminHeaders(adminSession)
        });
        setGifts(data.gifts);
      } else {
        const data = await requestJson<{ gifts: GiftItem[] }>("/api/gifts", {
          headers: guestHeaders(guestSession)
        });
        setGifts(data.gifts);
      }
    } catch (error) {
      showError(error);
    } finally {
      setLoading(false);
    }
  }

  async function refreshReservations(session = guestSession) {
    if (!session) return;
    try {
      const data = await requestJson<{ gifts: GiftItem[] }>("/api/profile/reservations", {
        headers: guestHeaders(session)
      });
      setReservations(data.gifts);
    } catch (error) {
      showError(error);
    }
  }

  async function submitAuth(
    authMode: AuthMode,
    payload: { name: string; username: string; password: string }
  ) {
    setBusy("auth");
    try {
      if (authMode === "register") {
        const session = await requestJson<UserSession>("/api/auth/register", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload)
        });
        localStorage.setItem(guestStorageKey, JSON.stringify(session));
        setGuestSession(session);
        setMode("guest");
        setToast({ kind: "ok", text: `Готово, ${session.name}` });
        return;
      }

      try {
        const session = await requestJson<UserSession>("/api/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ username: payload.username, password: payload.password })
        });
        localStorage.setItem(guestStorageKey, JSON.stringify(session));
        setGuestSession(session);
        setMode("guest");
        setToast({ kind: "ok", text: `Привет, ${session.name}` });
        return;
      } catch {
        const session = await requestJson<UserSession>("/api/admin/session", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ username: payload.username, password: payload.password })
        });
        localStorage.setItem(adminStorageKey, JSON.stringify(session));
        setAdminSession(session);
        setMode("admin");
        setToast({ kind: "ok", text: "Админ вошёл" });
      }
    } catch {
      setToast({ kind: "error", text: "Неверный логин или пароль" });
    } finally {
      setBusy(null);
    }
  }

  async function reserveGift(gift: GiftItem) {
    if (!guestSession) {
      setToast({ kind: "error", text: "Сначала войди" });
      return;
    }
    setBusy(`reserve-${gift.id}`);
    try {
      await requestJson(`/api/gifts/${gift.id}/reserve`, {
        method: "POST",
        headers: guestHeaders(guestSession)
      });
      setToast({ kind: "ok", text: "Подарок забронирован" });
      await Promise.all([refreshGifts(), refreshReservations(guestSession)]);
    } catch (error) {
      showError(error);
      await refreshGifts();
    } finally {
      setBusy(null);
    }
  }

  async function releaseGift(gift: GiftItem) {
    if (!guestSession) return;
    setBusy(`release-${gift.id}`);
    try {
      await requestJson(`/api/gifts/${gift.id}/reserve`, {
        method: "DELETE",
        headers: guestHeaders(guestSession)
      });
      setToast({ kind: "ok", text: "Бронь снята" });
      await Promise.all([refreshGifts(), refreshReservations(guestSession)]);
    } catch (error) {
      showError(error);
    } finally {
      setBusy(null);
    }
  }

  async function addGift(form: HTMLFormElement) {
    setBusy("add-gift");
    try {
      const formData = new FormData(form);
      await requestJson("/api/admin/gifts", {
        method: "POST",
        headers: adminHeaders(adminSession),
        body: formData
      });
      form.reset();
      setToast({ kind: "ok", text: "Подарок добавлен" });
      await refreshGifts();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(null);
    }
  }

  async function deleteGift(gift: GiftItem) {
    setBusy(`delete-${gift.id}`);
    try {
      await requestJson(`/api/admin/gifts/${gift.id}`, {
        method: "DELETE",
        headers: adminHeaders(adminSession)
      });
      setToast({ kind: "ok", text: "Подарок удалён" });
      await refreshGifts();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(null);
    }
  }

  function logoutGuest() {
    localStorage.removeItem(guestStorageKey);
    setGuestSession(null);
    setReservations([]);
    setGifts([]);
    setToast({ kind: "ok", text: "Гость вышел" });
  }

  function logoutAdmin() {
    localStorage.removeItem(adminStorageKey);
    setAdminSession(null);
    setGifts([]);
    setMode("guest");
    setToast({ kind: "ok", text: "Админ вышел" });
  }

  function showError(error: unknown) {
    setToast({ kind: "error", text: error instanceof Error ? error.message : "Что-то пошло не так" });
  }

  return (
    <div className={needsAuth ? "app authOnly" : "app"}>
      {toast && (
        <div className={`toast ${toast.kind}`} role="status">
          {toast.kind === "ok" ? <Check size={18} /> : <X size={18} />}
          <span>{toast.text}</span>
        </div>
      )}

      {needsAuth ? (
        <AuthGate busy={busy} onSubmit={submitAuth} />
      ) : (
        <>
          <header className="topBand">
            <PhotoRail />

            <div className="masthead">
              <div className="brandBlock">
                <span className="eyebrow">День рождения</span>
                <h1>Vislist</h1>
              </div>
              {mode === "admin" && (
                <div className="adminBadge" title="Админский режим">
                  <Crown size={18} />
                  <span>Админ</span>
                </div>
              )}
            </div>
          </header>

          <main className="workspace">
            {mode === "guest" ? (
              <GuestView
                session={guestSession!}
                gifts={gifts}
                reservations={reservations}
                loading={loading}
                busy={busy}
                reservedCount={reservedCount}
                onLogout={logoutGuest}
                onReserve={reserveGift}
                onRelease={releaseGift}
                onRefresh={() => void Promise.all([refreshGifts(), refreshReservations()])}
              />
            ) : (
              <AdminView
                session={adminSession!}
                gifts={gifts}
                loading={loading}
                busy={busy}
                onLogout={logoutAdmin}
                onAddGift={addGift}
                onDeleteGift={deleteGift}
                onRefresh={() => void refreshGifts()}
              />
            )}
          </main>
        </>
      )}
    </div>
  );
}

function AuthGate(props: {
  busy: string | null;
  onSubmit: (
    authMode: AuthMode,
    payload: { name: string; username: string; password: string }
  ) => Promise<void>;
}) {
  return (
    <main className="authScreen">
      <section className="authWindow">
        <div className="authHeader">
          <div>
            <span className="eyebrow">День рождения</span>
            <h1>Vislist</h1>
          </div>
        </div>
        <AuthForm busy={props.busy === "auth"} onSubmit={props.onSubmit} />
      </section>
    </main>
  );
}

function AuthForm(props: {
  busy: boolean;
  onSubmit: (
    authMode: AuthMode,
    payload: { name: string; username: string; password: string }
  ) => Promise<void>;
}) {
  const [authMode, setAuthMode] = useState<AuthMode>("register");
  const [name, setName] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const canSubmit =
    username.trim().length >= 3 &&
    password.trim().length >= 4 &&
    (authMode === "login" || name.trim().length > 0);

  return (
    <form
      className="authPanel"
      onSubmit={(event) => {
        event.preventDefault();
        void props.onSubmit(authMode, { name, username, password });
      }}
    >
      <div className="authSwitch" aria-label="Вход">
        <button
          className={authMode === "register" ? "switchButton active" : "switchButton"}
          type="button"
          onClick={() => setAuthMode("register")}
        >
          <UserPlus size={17} />
          Регистрация
        </button>
        <button
          className={authMode === "login" ? "switchButton active" : "switchButton"}
          type="button"
          onClick={() => setAuthMode("login")}
        >
          <LogIn size={17} />
          Авторизация
        </button>
      </div>

      {authMode === "register" && (
        <label>
          <span>Имя</span>
          <input autoComplete="name" value={name} onChange={(event) => setName(event.target.value)} />
        </label>
      )}

      <label>
        <span>Логин</span>
        <input
          autoComplete="username"
          value={username}
          onChange={(event) => setUsername(event.target.value)}
        />
      </label>

      <label>
        <span>Пароль</span>
        <input
          autoComplete={authMode === "register" ? "new-password" : "current-password"}
          type="password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
      </label>

      <button className="primaryButton" type="submit" disabled={props.busy || !canSubmit}>
        {authMode === "register" ? <UserPlus size={18} /> : <LogIn size={18} />}
        {authMode === "register" ? "Создать" : "Войти"}
      </button>
    </form>
  );
}

function PhotoRail() {
  const railRef = useRef<HTMLDivElement | null>(null);

  function scrollPhotos(direction: -1 | 1) {
    const rail = railRef.current;
    if (!rail) return;
    rail.scrollBy({ left: direction * rail.clientWidth * 0.82, behavior: "smooth" });
  }

  return (
    <div className="photoRailShell">
      <button className="photoNav photoNavLeft" type="button" onClick={() => scrollPhotos(-1)} title="Назад">
        <ChevronLeft size={24} />
      </button>
      <div ref={railRef} className="photoRail" aria-label="Фото с прошлых дней рождений">
        {birthdayPhotos.map((src, index) => (
          <figure className="memoryCard" key={src}>
            <img src={src} alt={`День рождения ${index + 1}`} />
          </figure>
        ))}
      </div>
      <button className="photoNav photoNavRight" type="button" onClick={() => scrollPhotos(1)} title="Вперёд">
        <ChevronRight size={24} />
      </button>
    </div>
  );
}

function GuestView(props: {
  session: UserSession;
  gifts: GiftItem[];
  reservations: GiftItem[];
  loading: boolean;
  busy: string | null;
  reservedCount: number;
  onLogout: () => void;
  onReserve: (gift: GiftItem) => Promise<void>;
  onRelease: (gift: GiftItem) => Promise<void>;
  onRefresh: () => void;
}) {
  const { session, gifts, reservations, loading, busy, reservedCount } = props;

  return (
    <>
      <section className="accountBand">
        <AccountStrip
          session={session}
          counter={reservedCount}
          counterLabel="Брони"
          onRefresh={props.onRefresh}
          onLogout={props.onLogout}
        />
      </section>

      <section className="contentGrid">
        <div className="giftColumn">
          <div className="sectionHeader">
            <h2>Подарки</h2>
            <span>{loading ? "Загрузка" : `${gifts.length} шт.`}</span>
          </div>
          <GiftGrid
            gifts={gifts}
            busy={busy}
            role="guest"
            onReserve={props.onReserve}
            onRelease={props.onRelease}
          />
        </div>

        <aside className="profilePanel">
          <div className="sectionHeader">
            <h2>Мои брони</h2>
            <span>{reservations.length}</span>
          </div>
          {reservations.length > 0 ? (
            <div className="reservationList">
              {reservations.map((gift) => (
                <article className="reservationItem" key={gift.id}>
                  <GiftImage gift={gift} compact />
                  <div>
                    <strong>{gift.title}</strong>
                    {gift.linkUrl && (
                      <a href={gift.linkUrl} target="_blank" rel="noreferrer">
                        <ExternalLink size={14} />
                        Ссылка
                      </a>
                    )}
                  </div>
                </article>
              ))}
            </div>
          ) : (
            <EmptyState text="Пока пусто" />
          )}
        </aside>
      </section>
    </>
  );
}

function AdminView(props: {
  session: UserSession;
  gifts: GiftItem[];
  loading: boolean;
  busy: string | null;
  onLogout: () => void;
  onAddGift: (form: HTMLFormElement) => Promise<void>;
  onDeleteGift: (gift: GiftItem) => Promise<void>;
  onRefresh: () => void;
}) {
  return (
    <>
      <section className="accountBand">
        <AccountStrip
          session={props.session}
          counter={props.gifts.filter((gift) => gift.reserved).length}
          counterLabel="Занято"
          onRefresh={props.onRefresh}
          onLogout={props.onLogout}
        />
      </section>

      <section className="adminLayout">
        <AddGiftForm session={props.session} busy={props.busy === "add-gift"} onSubmit={props.onAddGift} />

        <div className="giftColumn">
          <div className="sectionHeader">
            <h2>Подарки админа</h2>
            <div className="headerActions">
              <span>{props.loading ? "Загрузка" : `${props.gifts.length} шт.`}</span>
              <button className="iconButton" type="button" onClick={props.onRefresh} title="Обновить">
                <RefreshCw size={18} />
              </button>
            </div>
          </div>
          <GiftGrid gifts={props.gifts} busy={props.busy} role="admin" onDelete={props.onDeleteGift} />
        </div>
      </section>
    </>
  );
}

function AccountStrip(props: {
  session: UserSession;
  counter: number;
  counterLabel: string;
  onRefresh: () => void;
  onLogout: () => void;
}) {
  return (
    <div className="accountStrip">
      <div className="accountName">
        <span className="eyebrow">{props.session.username}</span>
        <strong>{props.session.name}</strong>
      </div>
      <div className="identityActions">
        <span className="counter" title={props.counterLabel}>
          {props.counter}
        </span>
        <button className="iconButton" type="button" onClick={props.onRefresh} title="Обновить">
          <RefreshCw size={18} />
        </button>
        <button className="iconButton" type="button" onClick={props.onLogout} title="Выйти">
          <LogOut size={18} />
        </button>
      </div>
    </div>
  );
}

function AddGiftForm(props: { session: UserSession; busy: boolean; onSubmit: (form: HTMLFormElement) => Promise<void> }) {
  const [fileName, setFileName] = useState("");
  const [titleValue, setTitleValue] = useState("");
  const [linkValue, setLinkValue] = useState("");
  const [previewImageUrl, setPreviewImageUrl] = useState("");
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewMessage, setPreviewMessage] = useState("");
  const [previewOk, setPreviewOk] = useState(false);
  const linkInputRef = useRef<HTMLInputElement | null>(null);
  const previewRequest = useRef(0);

  async function fetchPreview(nextLink = linkValue) {
    const linkUrl = nextLink.trim();
    if (!linkUrl) {
      setPreviewImageUrl("");
      setPreviewMessage("");
      setPreviewOk(false);
      return;
    }
    const requestID = previewRequest.current + 1;
    previewRequest.current = requestID;
    setPreviewBusy(true);
    setPreviewMessage("Ищу фото и название на WB/Ozon");
    setPreviewOk(false);
    try {
      const data = await requestJson<{ linkUrl: string; imageUrl: string; title?: string }>("/api/admin/link-preview", {
        method: "POST",
        headers: { ...adminHeaders(props.session), "Content-Type": "application/json" },
        body: JSON.stringify({ linkUrl })
      });
      if (previewRequest.current !== requestID) return;
      setLinkValue(data.linkUrl);
      if (data.title) {
        setTitleValue(data.title);
      }
      setPreviewImageUrl(data.imageUrl || "");
      setPreviewMessage(
        data.imageUrl
          ? data.title
            ? "Фото и название подтянулись"
            : "Фото подтянулось"
          : "Название подтянулось, фото не найдено"
      );
      setPreviewOk(Boolean(data.imageUrl || data.title));
    } catch (error) {
      if (previewRequest.current !== requestID) return;
      setPreviewImageUrl("");
      setPreviewMessage(error instanceof Error ? error.message : "Не получилось подтянуть фото");
      setPreviewOk(false);
    } finally {
      if (previewRequest.current === requestID) {
        setPreviewBusy(false);
      }
    }
  }

  function clearPreview() {
    setPreviewImageUrl("");
    setPreviewMessage("");
    setPreviewOk(false);
  }

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    void props.onSubmit(event.currentTarget).then(() => {
      setFileName("");
      setTitleValue("");
      setLinkValue("");
      clearPreview();
    });
  }

  return (
    <form className="addGiftPanel" onSubmit={handleSubmit}>
      <div className="sectionHeader">
        <h2>Новый подарок</h2>
        <PackagePlus size={20} />
      </div>

      <label>
        <span>Название</span>
        <input name="title" required value={titleValue} onChange={(event) => setTitleValue(event.target.value)} />
      </label>

      <label>
        <span>Ссылка</span>
        <div className="linkRow">
          <input
            ref={linkInputRef}
            name="linkUrl"
            value={linkValue}
            inputMode="url"
            placeholder="WB, Ozon или ссылка на картинку"
            onBlur={() => void fetchPreview()}
            onChange={(event) => {
              setLinkValue(event.target.value);
              clearPreview();
            }}
            onPaste={() => {
              window.setTimeout(() => {
                void fetchPreview(linkInputRef.current?.value ?? "");
              }, 0);
            }}
          />
          <button
            className="secondaryButton previewButton"
            type="button"
            disabled={previewBusy || !linkValue.trim()}
            onClick={() => void fetchPreview()}
          >
            {previewBusy ? <RefreshCw size={17} /> : <ImagePlus size={17} />}
            Фото
          </button>
        </div>
      </label>

      <input name="previewImageUrl" type="hidden" value={previewImageUrl} readOnly />

      {(previewImageUrl || previewMessage) && (
        <div className={previewOk ? "linkPreview ok" : "linkPreview"}>
          {previewImageUrl && <img src={previewImageUrl} alt="Фото товара" />}
          <span>{previewMessage}</span>
        </div>
      )}

      <label>
        <span>Описание</span>
        <textarea name="description" rows={4} />
      </label>

      <label className="filePicker">
        <ImagePlus size={18} />
        <span>{fileName || "Фото подарка"}</span>
        <input
          name="image"
          type="file"
          accept="image/png,image/jpeg,image/webp,image/gif"
          onChange={(event) => setFileName(event.currentTarget.files?.[0]?.name ?? "")}
        />
      </label>

      <button className="primaryButton" type="submit" disabled={props.busy}>
        <PackagePlus size={18} />
        Добавить
      </button>
    </form>
  );
}

function GiftGrid(props: {
  gifts: GiftItem[];
  busy: string | null;
  role: "guest" | "admin";
  onReserve?: (gift: GiftItem) => Promise<void>;
  onRelease?: (gift: GiftItem) => Promise<void>;
  onDelete?: (gift: GiftItem) => Promise<void>;
}) {
  if (props.gifts.length === 0) {
    return <EmptyState text="Подарков пока нет" />;
  }

  return (
    <div className="giftGrid">
      {props.gifts.map((gift) => (
        <GiftCard
          key={gift.id}
          gift={gift}
          busy={props.busy}
          role={props.role}
          onReserve={props.onReserve}
          onRelease={props.onRelease}
          onDelete={props.onDelete}
        />
      ))}
    </div>
  );
}

function GiftCard(props: {
  gift: GiftItem;
  busy: string | null;
  role: "guest" | "admin";
  onReserve?: (gift: GiftItem) => Promise<void>;
  onRelease?: (gift: GiftItem) => Promise<void>;
  onDelete?: (gift: GiftItem) => Promise<void>;
}) {
  const { gift, role, busy } = props;
  const status = gift.reservedByMe ? "У тебя" : gift.reserved ? "Занято" : "Свободно";
  const statusClass = gift.reservedByMe ? "mine" : gift.reserved ? "reserved" : "free";

  return (
    <article className="giftCard">
      <GiftImage gift={gift} />

      <div className="giftBody">
        <div className="giftTitleRow">
          <h3>{gift.title}</h3>
          <span className={`statusPill ${statusClass}`}>{status}</span>
        </div>

        {gift.description && <p>{gift.description}</p>}

        <div className="giftActions">
          {gift.linkUrl && (
            <a className="secondaryButton" href={gift.linkUrl} target="_blank" rel="noreferrer">
              <ExternalLink size={17} />
              Ссылка
            </a>
          )}

          {role === "guest" && (
            <>
              {!gift.reserved && (
                <button
                  className="primaryButton"
                  type="button"
                  disabled={busy === `reserve-${gift.id}`}
                  onClick={() => void props.onReserve?.(gift)}
                >
                  <Check size={18} />
                  Забронировать
                </button>
              )}
              {gift.reservedByMe && (
                <button
                  className="dangerButton"
                  type="button"
                  disabled={busy === `release-${gift.id}`}
                  onClick={() => void props.onRelease?.(gift)}
                >
                  <X size={18} />
                  Снять
                </button>
              )}
            </>
          )}

          {role === "admin" && (
            <button
              className="dangerIconButton"
              type="button"
              disabled={busy === `delete-${gift.id}`}
              onClick={() => void props.onDelete?.(gift)}
              title="Удалить"
            >
              <Trash2 size={18} />
            </button>
          )}
        </div>
      </div>
    </article>
  );
}

function GiftImage(props: { gift: GiftItem; compact?: boolean }) {
  if (!props.gift.imageUrl) {
    return (
      <div className={props.compact ? "giftImage compact placeholder" : "giftImage placeholder"}>
        <Gift size={props.compact ? 18 : 32} />
      </div>
    );
  }
  return (
    <div className={props.compact ? "giftImage compact" : "giftImage"}>
      <img src={props.gift.imageUrl} alt={props.gift.title} />
    </div>
  );
}

function EmptyState(props: { text: string }) {
  return (
    <div className="emptyState">
      <Gift size={24} />
      <span>{props.text}</span>
    </div>
  );
}

function guestHeaders(session: UserSession | null): Record<string, string> {
  return session ? { "X-Guest-Token": session.token } : {};
}

function adminHeaders(session: UserSession | null): Record<string, string> {
  return session ? { "X-Admin-Token": session.token } : {};
}

async function requestJson<T = unknown>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init);
  if (!response.ok) {
    let message = "Запрос не прошёл";
    try {
      const data = (await response.json()) as { error?: string };
      message = data.error || message;
    } catch {
      message = response.statusText || message;
    }
    throw new Error(message);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return (await response.json()) as T;
}

function readStoredSession(key: string, role: UserSession["role"]): UserSession | null {
  const raw = localStorage.getItem(key);
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as UserSession;
    if (parsed.token && parsed.role === role && parsed.username && parsed.name) {
      return parsed;
    }
  } catch {
    localStorage.removeItem(key);
  }
  return null;
}

export default App;
