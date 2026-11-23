import { Injectable, Injector, inject } from '@angular/core';
import { CHAT_PROTOCOL } from './chat-protocol';
import { KonsertSocketService } from './konsert-socket.service';

export interface ChatSendFrame {
  session_id: string;
  chat_id: string;
  turn_id: string;
  content: string;
  attachments?: string[];
}

export interface ChatRespondFrame {
  chat_id: string;
  elicitation_id: string;
  option_id?: string;
  text?: string;
}

export interface ChatControlFrame {
  chat_id: string;
  cancel?: boolean;
  model?: string;
  effort?: string;
  approval_mode?: string;
}

/**
 * The only place that writes agent chat frames to the hub socket. Frame names come from
 * CHAT_PROTOCOL. Every method returns false when the socket is not open (nothing sent).
 */
@Injectable({ providedIn: 'root' })
export class ChatTransportService {
  // Resolved lazily: the socket service itself depends on the chat stores.
  private readonly injector = inject(Injector);
  private get socket(): KonsertSocketService {
    return this.injector.get(KonsertSocketService);
  }

  get isOpen(): boolean {
    return this.socket.isOpen;
  }

  send(frame: ChatSendFrame): boolean {
    return this.socket.sendFrame(CHAT_PROTOCOL.wsSend, frame);
  }

  respond(frame: ChatRespondFrame): boolean {
    return this.socket.sendFrame(CHAT_PROTOCOL.wsRespond, frame);
  }

  control(frame: ChatControlFrame): boolean {
    return this.socket.sendFrame(CHAT_PROTOCOL.wsControl, frame);
  }
}
