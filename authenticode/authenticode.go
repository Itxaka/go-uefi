package authenticode

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"hash"

	encasn1 "encoding/asn1"

	"github.com/foxboron/go-uefi/pkcs7"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/cryptobyte/asn1"
)

// OID

var (
	// PE/COFF signing specific
	OIDSpcIndirectDataContent = encasn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 4}
	OIDSpcPEImageDataObjID    = encasn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 15}
)

type Authenticode struct {
	Pkcs   *pkcs7.PKCS7
	Algid  *pkix.AlgorithmIdentifier
	Digest []byte
}

func (a *Authenticode) Verify(cert *x509.Certificate, img []byte) (bool, error) {
	var h hash.Hash
	switch {
	case a.Algid.Algorithm.Equal(pkcs7.OIDDigestAlgorithmSHA256):
		h = crypto.SHA256.New()
	default:
		return false, errors.New("unsupported hashing function")
	}

	if h.Size() != len(a.Digest) {
		return false, errors.New("wrong block size")
	}

	h.Write(img)
	digest := h.Sum(nil)
	if !bytes.Equal(digest, a.Digest) {
		return false, errors.New("incorrect digest")
	}
	return a.Pkcs.Verify(cert)
}

func CreateSpcIndirectDataContent(digest []byte, alg crypto.Hash) ([]byte, error) {
	var b cryptobyte.Builder

	//		data           SpcAttributeTypeAndOptionalValue,
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {

		//		type   ObjectID,
		b.AddASN1ObjectIdentifier(OIDSpcPEImageDataObjID)

		//	SpcPeImageData ::= SEQUENCE {
		b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {

			//		flags                   SpcPeImageFlags DEFAULT { includeResources },
			//	SpcPeImageFlags ::= BIT STRING {
			//		includeResources            (0),
			//		includeDebugInfo            (1),
			//		includeImportAddressTable   (2)
			//	}
			// We could also pass '0' but my reference implementation has one NULL byte, and not two NULL bytes.
			b.AddASN1BitString(nil)

			//		file                    SpcLink
			// SpcLink ::= CHOICE {
			//		url                     [0] IMPLICIT IA5STRING,
			//		moniker                 [1] IMPLICIT SpcSerializedObject,
			//		file                    [2] EXPLICIT SpcString
			//	} --#public--
			b.AddASN1(asn1.Tag(0).ContextSpecific().Constructed(), func(b *cryptobyte.Builder) {

				//		file                    [2] EXPLICIT SpcString
				b.AddASN1(asn1.Tag(2).ContextSpecific().Constructed(), func(b *cryptobyte.Builder) {

					//		unicode                 [0] IMPLICIT BMPSTRING,
					b.AddASN1(asn1.Tag(0).ContextSpecific(), func(b *cryptobyte.Builder) {

						// <<<Obsolete>>>, but with null bytes after each bytes. idk why.
						s := []byte{0x00, 0x3c, 0x00, 0x3c, 0x00, 0x3c, 0x00, 0x4f, 0x00, 0x62,
							0x00, 0x73, 0x00, 0x6f, 0x00, 0x6c, 0x00, 0x65, 0x00, 0x74,
							0x00, 0x65, 0x00, 0x3e, 0x00, 0x3e, 0x00, 0x3E}
						b.AddBytes(s)

					})
				})
			})
		})
	})

	//	DigestInfo ::= SEQUENCE {
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {

		//		digestAlgorithm  AlgorithmIdentifier,
		b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {

			//		digestAlgorithm  AlgorithmIdentifier,
			b.AddASN1ObjectIdentifier(pkcs7.OIDDigestAlgorithmSHA256)

			// Add explicit null, I believe it's needed by picky UEFI things
			b.AddASN1NULL()
		})

		//		digest           OCTETSTRING
		b.AddASN1OctetString(digest)
	})

	return b.Bytes()
}

func SignAuthenticode(signer crypto.Signer, cert *x509.Certificate, digest []byte, alg crypto.Hash) ([]byte, error) {
	h := alg.New()
	h.Write(digest)
	b, err := CreateSpcIndirectDataContent(h.Sum(nil), alg)
	if err != nil {
		return nil, fmt.Errorf("failed creating SpcIndirectDataContent: %v", err)
	}
	return pkcs7.SignPKCS7(signer, cert, OIDSpcIndirectDataContent, b)
}

func ParseAuthenticode(b []byte) (*Authenticode, error) {
	var auth Authenticode
	pkcs, err := pkcs7.ParsePKCS7(b)
	if err != nil {
		return nil, fmt.Errorf("failed parsing authenticode: %v", err)
	}

	if !pkcs.OID.Equal(OIDSpcIndirectDataContent) {
		return nil, fmt.Errorf("not an authenticode siganture")
	}

	auth.Pkcs = pkcs

	der := cryptobyte.String(pkcs.ContentInfo)
	if !der.ReadASN1(&der, asn1.SEQUENCE) {
		return nil, errors.New("no spcindirectdatacontent")
	}

	var spcdata cryptobyte.String
	if !der.ReadASN1(&spcdata, asn1.SEQUENCE) {
		return nil, errors.New("no spcindirectdatacontent")
	}

	var dtype encasn1.ObjectIdentifier
	if !spcdata.ReadASN1ObjectIdentifier(&dtype) {
		return nil, errors.New("missing objectid type")
	}

	if !dtype.Equal(OIDSpcPEImageDataObjID) {
		return nil, fmt.Errorf("incorrect, expected %v, got %v", OIDSpcIndirectDataContent, dtype)
	}

	// TODO: We don't care about the next stuff in spcdata
	if !spcdata.SkipASN1(asn1.SEQUENCE) {
		return nil, errors.New("no spcpeimagedata")
	}

	if !der.ReadASN1(&der, asn1.SEQUENCE) {
		return nil, errors.New("no spcindirectdatacontent")
	}

	algid, err := pkcs7.ParseAlgorithmIdentifier(&der)
	if err != nil {
		return nil, fmt.Errorf("failed parsing DigestInfo: %v", err)
	}
	auth.Algid = algid

	var digest cryptobyte.String
	if !der.ReadASN1(&digest, asn1.OCTET_STRING) {
		return nil, errors.New("no spcindirectdatacontent")
	}

	auth.Digest = digest

	return &auth, err
}
